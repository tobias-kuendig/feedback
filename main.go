package main

//go:generate nix run github:a-h/templ generate

import (
	"database/sql"
	"errors"
	"feedback/templates"
	"github.com/pocketbase/pocketbase/daos"
	"log"
	"math/rand"
	"net/http"
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
			err = form.LoadData(map[string]any{
				"title":       c.Request().FormValue("title"),
				"valid_until": c.Request().FormValue("valid_until"),
				"slug":        slug,
				"pin":         randomNumber(100_000, 999_999),
				"password":    randomString(8),
			})
			if err != nil {
				return err
			}

			// validate and submit (internally it calls app.Dao().SaveRecord(record) in a transaction)
			if err = form.Submit(); err != nil {
				return err
			}

			return c.Redirect(http.StatusFound, "/s/"+slug+"?created")
		}, apis.ActivityLogger(app))

		e.Router.GET("/s/:slug", func(c echo.Context) error {
			space, err := app.Dao().FindFirstRecordByData("spaces", "slug", c.PathParam("slug"))
			if err != nil {
				return err
			}

			questions, err := app.Dao().FindRecordsByExpr(
				"questions",
				dbx.NewExp("space_id = {:space_id} ORDER BY sort_order", dbx.Params{"space_id": space.Id}),
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

			return Render(c, http.StatusOK, templates.Feedback(space, questions, choicesByQuestion))
		})

		e.Router.POST("/s/:slug/question", func(c echo.Context) error {
			space, err := app.Dao().FindFirstRecordByData("spaces", "slug", c.PathParam("slug"))
			if err != nil {
				return err
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
				question.Set("space_id", space.Id)
				question.Set("text", c.Request().FormValue("text"))
				question.Set("type", c.Request().FormValue("type"))
				question.Set("sort_order", result.MaxSortOrder+1)

				if err = txDao.SaveRecord(question); err != nil {
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

				for i, choice := range createChoices {
					choiceRecord := models.NewRecord(choices)
					choiceRecord.Set("question_id", question.Id)
					choiceRecord.Set("sort_order", i)
					choiceRecord.Set("text", choice)

					if err = txDao.SaveRecord(choiceRecord); err != nil {
						return err
					}
				}

				return nil
			})
			if err != nil {
				return err
			}

			return c.Redirect(http.StatusFound, "/s/"+c.PathParam("slug")+"?created")

		})

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
