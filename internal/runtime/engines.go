package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/provider"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// engineFacts are what KairoCode knows of an engine (ADR 0007, 0014,
// 0015), in one place: each question about an engine is answered here.
type engineFacts struct {
	// agent is its agent's id in ACP's registry; "" if it is not one.
	agent string
	// turn is the action that runs its turns; "" for KairoCode's own loop.
	turn string
	// dry: every tool it calls goes through KairoCode's, so a try of a
	// flow may run its agents on it (ADR 0017).
	dry bool
	// schema: it answers a flow's decide itself, with a schema; one that
	// does not has the API or Claude Code decide for it (ADR 0014).
	schema bool
}

// factsOf is what is known of engine ("": the API's).
func factsOf(engine string) engineFacts {
	switch {
	case engine == "" || engine == workflows.EngineAPI:
		return engineFacts{dry: true, schema: true}
	case engine == workflows.EngineClaudeCode:
		return engineFacts{turn: workflows.ActClaudeTurn, dry: true, schema: true}
	case engine == workflows.EngineCodex:
		return engineFacts{agent: codexAgent, turn: workflows.ActCodexTurn}
	case engine == workflows.EngineCopilot:
		return engineFacts{agent: copilotAgent, turn: workflows.ActCopilotTurn, dry: true}
	case strings.HasPrefix(engine, workflows.EngineOAI) && engine != workflows.EngineOAI:
		// An OpenAI-compatible API, in KairoCode's own loop (ADR 0035).
		return engineFacts{dry: true, schema: true}
	case strings.HasPrefix(engine, workflows.EngineACP) && engine != workflows.EngineACP:
		agent := strings.TrimPrefix(engine, workflows.EngineACP)
		return engineFacts{agent: agent, turn: workflows.ActACPTurn, dry: agent == copilotAgent}
	}
	return engineFacts{}
}

// registryAgent is the registry id of an ACP engine; "" if engine is not one.
func registryAgent(engine string) string { return factsOf(engine).agent }

// dryEngine reports whether a try of a flow may run its agents on engine:
// one whose every tool goes through KairoCode's.
func dryEngine(engine string) bool { return factsOf(engine).dry }

// cliTurnActions are the actions that run a CLI engine's turn.
var cliTurnActions = []string{workflows.ActClaudeTurn, workflows.ActCodexTurn, workflows.ActCopilotTurn, workflows.ActACPTurn}

// cliTurnPlan reports whether plan is a run of a CLI engine's turn.
func cliTurnPlan(plan string) bool {
	a, ok := strings.CutPrefix(plan, "kairo.call/")
	return ok && slices.Contains(cliTurnActions, a)
}

// engineModel checks engine (as named: api, a CLI, acp:<agent>) and gives
// the model it runs with: the API's needs a provider, and has its default
// model unless one is named; a CLI's or an agent's is its own unless named.
func engineModel(engine, model string, hasProvider bool) (string, error) {
	switch {
	case engine == workflows.EngineAPI:
		if !hasProvider {
			return "", errNoAPIProvider
		}
		if model == "" {
			model = provider.DefaultModel
		}
	case strings.HasPrefix(engine, workflows.EngineOAI) && engine != workflows.EngineOAI:
		// Its model, if none is named, is its settings' (engineFor).
	case factsOf(engine).turn == "":
		return "", fmt.Errorf("unknown engine %q", engine)
	}
	return model, nil
}

// errNoAPIProvider: the api engine was asked for, and there is no model
// provider for it.
var errNoAPIProvider = errors.New("this KairoCode has no model provider for the api engine")
