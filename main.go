package main

//go:generate nix run github:a-h/templ generate

import (
	"database/sql"
	"errors"
	"feedback/templates"
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/daos"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/forms"
	"github.com/pocketbase/pocketbase/models"
)

func main() {
	app := pocketbase.New()

	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.Use(middleware.BodyLimit(500_000)) // max ~500kb body

		e.Router.GET(
			"/",
			func(c echo.Context) error {
				return Render(c, http.StatusOK, templates.Index())
			},
			middleware.Gzip(),
		)

		e.Router.POST("/feedback", func(c echo.Context) error {
			collection, err := app.Dao().FindCollectionByNameOrId("spaces")
			if err != nil {
				return err
			}

			form := forms.NewRecordUpsert(app, models.NewRecord(collection))

			slug := slugify(strings.TrimSpace(c.Request().FormValue("title")) + "-" + randomString(4))

			// or form.LoadRequest(r, "")
			password := randomString(8)
			err = form.LoadData(map[string]any{
				"title":       c.Request().FormValue("title"),
				"valid_until": c.Request().FormValue("valid_until"),
				"slug":        slug,
				"pin":         randomNumber(100_000, 999_999),
				"password":    password,
			})
			if err != nil {
				return err
			}

			// validate and submit (internally it calls app.Dao().SaveRecord(record) in a transaction)
			if err = form.Submit(); err != nil {
				return err
			}

			return c.Redirect(http.StatusFound, "/s/"+slug+"?created&password="+password)
		}, apis.ActivityLogger(app))

		e.Router.GET("/s/:slug", func(c echo.Context) error {
			space, err := app.Dao().FindFirstRecordByData("spaces", "slug", c.PathParam("slug"))
			if err != nil {
				return err
			}

			questions, err := app.Dao().FindRecordsByExpr(
				"questions",
				dbx.NewExp("space_id = {:space_id} ORDER BY sort_order DESC", dbx.Params{"space_id": space.Id}),
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			choicesByQuestion := make(map[string][]*models.Record)
			questionIDs := make([]any, len(questions))

			for i, question := range questions {
				questionIDs[i] = question.Id
			}

			var choices []*models.Record
			query := app.Dao().RecordQuery("choices").
				AndWhere(dbx.In("question_id", questionIDs...)).
				OrderBy("sort_order DESC").
				Limit(100)

			if err = query.All(&choices); err != nil {
				return err
			}

			for _, choice := range choices {
				questionID := choice.GetString("question_id")
				choicesByQuestion[questionID] = append(choicesByQuestion[questionID], choice)
			}

			var answers []*models.Record
			query = app.Dao().RecordQuery("answers").
				AndWhere(dbx.In("question_id", questionIDs...)).
				OrderBy("created DESC").
				Limit(100)

			if err = query.All(&answers); err != nil {
				return err
			}

			answersByQuestion := make(map[string][]*models.Record)
			for _, answer := range answers {
				questionID := answer.GetString("question_id")
				answersByQuestion[questionID] = append(answersByQuestion[questionID], answer)
			}

			return Render(c, http.StatusOK, templates.Feedback(space, questions, choicesByQuestion, answersByQuestion, c.QueryParam("password")))
		})

		e.Router.POST("/s/:slug/question", func(c echo.Context) error {
			space, err := app.Dao().FindFirstRecordByData("spaces", "slug", c.PathParam("slug"))
			if err != nil {
				return err
			}
			password := c.FormValue("password")

			if space.GetString("password") != password {
				return apis.NewApiError(http.StatusBadRequest, "Invalid password", nil)
			}

			questions, err := app.Dao().FindCollectionByNameOrId("questions")
			if err != nil {
				return err
			}
			choices, err := app.Dao().FindCollectionByNameOrId("choices")
			if err != nil {
				return err
			}

			// get the next sort order
			var result struct {
				MaxSortOrder int `db:"max_sort_order"`
			}
			err = app.Dao().DB().
				Select("IFNULL(MAX(sort_order), 0) as max_sort_order").
				From("questions").
				Where(dbx.NewExp("space_id = {:space_id}", dbx.Params{"space_id": space.Id})).
				One(&result)
			if err != nil {
				return err
			}

			err = app.Dao().RunInTransaction(func(txDao *daos.Dao) error {
				question := models.NewRecord(questions)

				form := forms.NewRecordUpsert(app, question)
				form.SetDao(txDao)

				err = form.LoadData(map[string]any{
					"space_id":   space.Id,
					"text":       strings.TrimSpace(c.Request().FormValue("text")),
					"type":       c.Request().FormValue("type"),
					"sort_order": result.MaxSortOrder + 1,
				})
				if err != nil {
					return err
				}

				if err = form.Submit(); err != nil {
					return err
				}

				err = c.Request().ParseForm()
				if err != nil {
					return err
				}

				if c.Request().FormValue("type") == "textarea" {
					return nil
				}

				createChoices, ok := c.Request().PostForm["choices[]"]
				if !ok {
					return nil
				}

				var hasValidChoice bool
				for i, choice := range createChoices {
					if choice == "" {
						continue
					}

					choiceRecord := models.NewRecord(choices)

					choiceForm := forms.NewRecordUpsert(app, choiceRecord)
					choiceForm.SetDao(txDao)

					err = choiceForm.LoadData(map[string]any{
						"question_id": question.Id,
						"sort_order":  i + 1,
						"text":        strings.TrimSpace(choice),
					})
					if err != nil {
						return err
					}

					if err = choiceForm.Submit(); err != nil {
						return err
					}

					hasValidChoice = true
				}

				if !hasValidChoice {
					return apis.NewApiError(http.StatusBadRequest, "At least one choice is required.", map[string]validation.Error{
						"choices": validation.NewError("required", "At least one choice is required."),
					})
				}

				return nil
			})
			if err != nil {
				return err
			}

			if c.Request().Header.Get("HX-Request") == "true" {
				return Render(c, http.StatusOK, templates.Form(space, password))
			}

			return c.Redirect(http.StatusFound, "/s/"+c.PathParam("slug")+"?created")

		}, apis.ActivityLogger(app))

		e.Router.POST("/s/:slug/answer", func(c echo.Context) error {
			collection, err := app.Dao().FindCollectionByNameOrId("answers")
			if err != nil {
				return err
			}

			err = c.Request().ParseForm()
			if err != nil {
				return err
			}

			err = app.Dao().RunInTransaction(func(txDao *daos.Dao) error {
				createAnswers, ok := c.Request().PostForm["text[]"]
				if !ok {
					return nil
				}

				for _, text := range createAnswers {
					record := models.NewRecord(collection)

					form := forms.NewRecordUpsert(app, record)
					form.SetDao(txDao)

					err = form.LoadData(map[string]any{
						"question_id": c.Request().FormValue("question_id"),
						"text":        strings.TrimSpace(text),
					})

					if err != nil {
						return err
					}

					if err = form.Submit(); err != nil {
						return err
					}
				}

				return nil
			})
			if err != nil {
				return err
			}

			if c.Request().Header.Get("HX-Request") == "true" {
				return c.String(http.StatusOK, "")
			}

			return c.Redirect(http.StatusFound, "/s/"+c.PathParam("slug")+"?answered")
		}, apis.ActivityLogger(app))

		e.Router.GET("/*", apis.StaticDirectoryHandler(os.DirFS("./pb_public"), false))

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// randomString generates a random string of length i.
func randomString(i int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, i)
	for j := range b {
		b[j] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func randomNumber(min, max int) int {
	return min + rand.Intn(max-min)
}

var reSlug = regexp.MustCompile(`[^-A-Za-z0-9]+`)

// slugify converts a string to a URL-friendly slug.
func slugify(value string) string {
	// replace spaces with dashes
	// remove non-alphanumeric characters
	// convert to lower case
	return strings.ToLower(reSlug.ReplaceAllString(strings.TrimSpace(value), "-"))
}

// Render replaces Echo's echo.Context.Render() with templ's templ.Component.Render().
func Render(ctx echo.Context, statusCode int, t templ.Component) error {
	buf := templ.GetBuffer()
	defer templ.ReleaseBuffer(buf)

	if err := t.Render(ctx.Request().Context(), buf); err != nil {
		return err
	}

	return ctx.HTML(statusCode, buf.String())
}
