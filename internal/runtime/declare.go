package runtime

import (
	"time"

	kairo "github.com/r-hashi01/kairo/sdk/go"

	"github.com/r-hashi01/kairocode/internal/tools"
	"github.com/r-hashi01/kairocode/internal/workflows"
)

// effect is how kairo treats a tool of effect e (AGENTS.md, invariant 3).
func effect(e tools.Effect) (kairo.Effect, []kairo.ActionOption) {
	switch e {
	case tools.Read:
		return kairo.Unprotected, nil
	case tools.Idempotent:
		return kairo.Real, []kairo.ActionOption{kairo.IdempotentRetry()}
	default:
		return kairo.Real, nil
	}
}

func (r *Runtime) declare() {
	switch v := r.cfg.onlyVersion; v {
	case workflows.SessionVersion6:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV6, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, r.turnWorkflow(), kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion5:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV5, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion4:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV4, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurn, kairo.WorkflowVersion(workflows.TurnVersion))
	case workflows.SessionVersion3, workflows.SessionVersion2:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV3, kairo.WorkflowVersion(v))
		kairo.Workflow(r.K, workflows.Turn, workflows.RunTurnV3, kairo.WorkflowVersion(v))
	default:
		kairo.Workflow(r.K, workflows.Session, workflows.RunSession, kairo.WorkflowVersion(workflows.SessionVersion))
		kairo.Workflow(r.K, workflows.Turn, r.turnWorkflow(), kairo.WorkflowVersion(workflows.TurnVersion))
		// Version 6 finishes as it started: a turn is taken as its result
		// said (kairo#3).
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV6, kairo.WorkflowVersion(workflows.SessionVersion6), kairo.Draining())
		// Version 5 finishes as it started: no switch of engine or model.
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV5, kairo.WorkflowVersion(workflows.SessionVersion5), kairo.Draining())
		// Version 4 finishes as it started: a stopped turn is not kept.
		kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV4, kairo.WorkflowVersion(workflows.SessionVersion4), kairo.Draining())
		// Versions 3 and 2 finish with the functions they started with.
		for _, v := range []string{workflows.SessionVersion3, workflows.SessionVersion2} {
			kairo.Workflow(r.K, workflows.Session, workflows.RunSessionV3, kairo.WorkflowVersion(v), kairo.Draining())
		}
		for _, v := range []string{workflows.TurnVersion3, workflows.TurnVersion2} {
			kairo.Workflow(r.K, workflows.Turn, workflows.RunTurnV3, kairo.WorkflowVersion(v), kairo.Draining())
		}
	}
	kairo.Workflow(r.K, workflows.MCPCall, workflows.RunMCPCall, kairo.WorkflowVersion(workflows.MCPCallVersion))
	if r.cfg.onlyFlowVersion == workflows.FlowVersion1 {
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV1, kairo.WorkflowVersion(workflows.FlowVersion1))
	} else {
		run := workflows.RunFlow
		if r.cfg.flowFunc != nil {
			run = r.cfg.flowFunc
		}
		kairo.Workflow(r.K, workflows.Flow, run, kairo.WorkflowVersion(workflows.FlowVersion))
		// Versions 2 and 1 finish as they started: no flow.step, and in 1
		// no At either.
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV2, kairo.WorkflowVersion(workflows.FlowVersion2), kairo.Draining())
		kairo.Workflow(r.K, workflows.Flow, workflows.RunFlowV1, kairo.WorkflowVersion(workflows.FlowVersion1), kairo.Draining())
	}
	// flow.step may cancel what a run before left (ADR 0003, addendum
	// 2026-11-04): a cancel done again does nothing, so it stays unprotected.
	kairo.Action(r.K, workflows.ActFlowStep, kairo.Unprotected, r.flowStep)
	kairo.Action(r.K, workflows.ActFlowLoad, kairo.Unprotected, r.flowLoad)
	kairo.Action(r.K, workflows.ActFlowWould, kairo.Unprotected, r.flowWould)
	// bash_wait (ADR 0028): watching a command and reading it change
	// nothing; done again, they find the same.
	kairo.Action(r.K, workflows.ActJobWatch, kairo.Unprotected, r.jobWatch)
	kairo.Action(r.K, workflows.ActJobResult, kairo.Unprotected, r.jobResult)
	kairo.Action(r.K, workflows.ActFlowPut, kairo.Unprotected, r.flowPut)
	kairo.Action(r.K, workflows.ActFlowText, kairo.Unprotected, r.flowText)
	kairo.Action(r.K, workflows.ActFlowAsk, kairo.Unprotected,
		func(*kairo.TaskContext, workflows.FlowAskInput) (bool, error) { return true, nil }) // recorded: Pending reads it
	kairo.Action(r.K, workflows.ActFlowDecide, kairo.Unprotected, r.flowDecide, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHookTrust, kairo.Unprotected, r.hookTrustAction) // a record, written once
	kairo.Action(r.K, workflows.ActHookCheck, kairo.Unprotected, r.hookCheckAction)
	kairo.Action(r.K, workflows.ActHookDecide, kairo.Unprotected, r.hookDecideAction) // a record, written once
	kairo.Action(r.K, workflows.ActHookCommand, kairo.Real, r.hookCommandAction)      // a command: not run again unasked
	kairo.Action(r.K, workflows.ActHookNote, kairo.Unprotected, r.hookNoteAction)
	kairo.Action(r.K, workflows.ActTaskArgs, kairo.Unprotected, r.taskArgs)
	kairo.Action(r.K, workflows.ActAskUserArgs, kairo.Unprotected, r.askUserArgs)
	kairo.Action(r.K, workflows.ActAskUserResult, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.TaskResultInput) (workflows.ToolOutput, error) {
			return r.toolResult(t, in.Call, tools.Result{Output: in.Text, IsError: in.IsError})
		})
	kairo.Action(r.K, workflows.ActTaskResult, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.TaskResultInput) (workflows.ToolOutput, error) {
			return r.toolResult(t, in.Call, tools.Result{Output: in.Text, IsError: in.IsError})
		})
	kairo.Action(r.K, workflows.ActClaudeTurn, kairo.Unprotected, r.claudeTurn, kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActCodexTurn, kairo.Unprotected, r.acpTurn(workflows.EngineCodex), kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActCopilotTurn, kairo.Unprotected, r.acpTurn(workflows.EngineCopilot), kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActACPTurn, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ClaudeTurnInput) (workflows.ClaudeTurnOutput, error) {
			return r.acpTurn(in.Engine)(t, in)
		}, kairo.Timeout(time.Hour))
	kairo.Action(r.K, workflows.ActPermissionCheck, kairo.Unprotected, r.permissionCheck)
	kairo.Action(r.K, workflows.ActToolDenied, kairo.Unprotected, r.toolDenied)
	kairo.Action(r.K, workflows.ActMCPAsk, kairo.Unprotected, r.mcpAsk)
	kairo.Action(r.K, workflows.ActMCPGrant, kairo.Unprotected, r.mcpGrant)

	kairo.Action(r.K, workflows.ActAgentLoad, kairo.Unprotected, r.agentLoad)
	kairo.Action(r.K, workflows.ActModel, kairo.Unprotected, r.model, kairo.Timeout(10*time.Minute))
	kairo.Action(r.K, workflows.ActHistoryExtend, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.HistoryExtendInput) (string, error) {
			return r.Store.Extend(t, in.Prev, in.Messages)
		})
	kairo.Action(r.K, workflows.ActHistoryCancelled, kairo.Unprotected, r.historyCancelled)
	kairo.Action(r.K, workflows.ActHistoryCompact, kairo.Unprotected, r.historyCompact, kairo.Timeout(10*time.Minute))
	// External tools (ADR 0022): listed as the turn starts (a server that
	// lists its tools again lists the same, as far as KairoCode can tell);
	// run by their declared effect: read, or real (invariant 3).
	kairo.Action(r.K, workflows.ActExtLoad, kairo.Unprotected, r.extLoad, kairo.Timeout(2*time.Minute))
	kairo.Action(r.K, workflows.ActToolExtRead, kairo.Unprotected, r.extTool)
	kairo.Action(r.K, workflows.ActToolExtReal, kairo.Real, r.extTool)
	kairo.Action(r.K, workflows.ActTurnSettled, kairo.Unprotected, r.turnSettled) // it reads kairo's record
	kairo.Action(r.K, workflows.ActToolMissing, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			call, err := r.toolCall(t, in)
			if err != nil {
				return workflows.ToolOutput{}, err
			}
			return r.toolResult(t, in.Call, tools.Result{Output: "no such tool: " + call.Name, IsError: true})
		})
	kairo.Action(r.K, workflows.ActToolSkipped, kairo.Unprotected,
		func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			return r.toolResult(t, in.Call, tools.Result{Output: "not run: the reply that asked for it was cut off", IsError: true})
		})
	for _, d := range r.toolDefs() {
		if d.Plan != nil {
			kairo.Action(r.K, workflows.ActToolPrefix+d.Name+workflows.ActPlanSuffix, kairo.Unprotected,
				func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.PlanOutput, error) {
					return r.planTool(t, d, in)
				})
			if d.Apply != nil {
				eff, opts := effect(d.Effect)
				kairo.Action(r.K, workflows.ActToolPrefix+d.Name+workflows.ActApplySuffix, eff,
					func(t *kairo.TaskContext, in workflows.PlanApplyInput) (workflows.ToolOutput, error) {
						return r.applyPlan(t, d, in)
					}, opts...)
			}
			continue
		}
		eff, opts := effect(d.Effect)
		kairo.Action(r.K, workflows.ActToolPrefix+d.Name, eff, func(t *kairo.TaskContext, in workflows.ToolInput) (workflows.ToolOutput, error) {
			return r.runTool(t, d, in)
		}, opts...)
	}
	eff, opts := effect(tools.Idempotent)
	kairo.Action(r.K, workflows.ActFileApply, eff, r.applyWrite, opts...)
	// A snapshot writes only to its store; a restore makes the files what a
	// tree has, the same however often it runs.
	kairo.Action(r.K, workflows.ActSnapshot, eff, r.snapshotAction, opts...)
	kairo.Action(r.K, workflows.ActRestore, eff, r.restoreAction, opts...)
}

// turnWorkflow is the turn's function: RunTurn, or a test's stand-in.
func (r *Runtime) turnWorkflow() func(*kairo.Context, workflows.TurnInput) (workflows.TurnOutput, error) {
	if r.cfg.turn != nil {
		return r.cfg.turn
	}
	return workflows.RunTurn
}
