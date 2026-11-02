package workflows

import (
	"slices"

	kairo "github.com/r-hashi01/kairo/sdk/go"
)

// askUser asks the user an ask_user call's question (ADR 0018, 7), as a
// flow's question of id in this run, waits for the answer, and answers the
// call with it. A call that is not a question's goes back to the model.
func askUser(ctx *kairo.Context, id string, call ToolInput) (ToolOutput, error) {
	q, err := kairo.Call[AskUserArgs](ctx, ActAskUserArgs, call)
	if err != nil {
		return ToolOutput{}, err
	}
	if q.Error != "" {
		return kairo.Call[ToolOutput](ctx, ActAskUserResult, TaskResultInput{Call: call.Call, Text: q.Error, IsError: true})
	}
	answer, err := askAndWait(ctx, id, q.Question, q.Options)
	if err != nil {
		return ToolOutput{}, err
	}
	return kairo.Call[ToolOutput](ctx, ActAskUserResult, TaskResultInput{Call: call.Call, Text: answerText(answer)})
}

// askAndWait asks the user question, with options and AskUserElse to
// choose from, and waits for the answer (kairo.WaitFor: the process may
// stop meanwhile, invariant 7).
func askAndWait(ctx *kairo.Context, id, question string, options []string) (string, error) {
	opts := append(slices.Clone(options), AskUserElse)
	if _, err := kairo.Call[bool](ctx, ActFlowAsk, FlowAskInput{ID: id, Question: question, Options: opts}); err != nil {
		return "", err
	}
	a, err := kairo.WaitFor[FlowAnswer](ctx, SignalAnswerPrefix+id)
	return a.Option, err
}

// answerText is the user's answer, as the model is told it.
func answerText(answer string) string {
	if answer == AskUserElse {
		return "The user will answer in their own words, in their next message: end your turn now, without other calls."
	}
	return "The user answered: " + answer
}
