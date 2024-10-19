import PocketBase from '../vendor/pocketbase.es.mjs'

const pb = new PocketBase();

const space = JSON.parse(document.getElementById('space').textContent);

pb.collection("questions").subscribe("*", e => {
    if (e.action === 'create') {
        Alpine.store('space').questions.unshift(e.record)
    }
}, { headers: { 'X-Space': space.id }});

pb.collection("choices").subscribe("*", e => {
    if (e.action === 'create') {
        let choices = Alpine.store('space').choicesByQuestion[e.record.question_id]
        if (!Array.isArray(choices)) {
            choices = []
        }

        choices.push(e.record)

        Alpine.store('space').choicesByQuestion[e.record.question_id] = choices
    }
}, { headers: { 'X-Space': space.id }});

pb.collection("answers").subscribe("*", e => {
    if (e.action === 'create') {
        let answers = Alpine.store('space').answersByQuestion[e.record.question_id]
        if (!Array.isArray(answers)) {
            answers = []
        }

        answers.unshift(e.record)

        Alpine.store('space').answersByQuestion[e.record.question_id] = answers
    }
}, { headers: { 'X-Space': space.id }});

export default pb