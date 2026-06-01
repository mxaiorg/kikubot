package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/services"
	"log"
)

// ── Set Status Tool ──────────────────────────────────────────────────────────

func SetTaskStatusTool() ToolDefinition {
	return ToolDefinition{
		Name:        "set_task_status",
		Description: "Set the status of the agent to 'waiting', 'complete', or 'error'.  If fulfillment of a task requested by the user is waiting on other tasks or email responses, set the status to 'waiting'. If a final response has been returned to the manager, set the status to 'complete'.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"status": {
					"type": "string",
					"enum": ["waiting", "complete", "error"],
					"description": "The status to set the task to"
				},
				"message-id": {
					"type": "string",
					"description": "The Message-Id to associate with the status update"
				}
			}
		}`),
		Execute: setTaskStatus,
	}
}

func setTaskStatus(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Status    string `json:"status"`
		MessageId string `json:"message-id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("failed to parse setTaskStatus input: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// Forcing function for the "agent wrote a reply only as assistant text,
	// never invoked a send tool" failure mode. If this invocation hasn't
	// successfully delivered any outbound mail, refuse to mark the task as
	// waiting or complete — those states would otherwise leave the recipient
	// silent and the agent (or its upstream caller) stuck waiting for a
	// reply to a message that was never sent.
	//
	// The error tool_result feeds back to the model, which on the next turn
	// typically corrects by calling the appropriate send tool first.
	//
	// Caveats:
	//   - Gate active only when WithSendTracker decorated ctx. CLI tools,
	//     tests, and ad-hoc scripts that invoke set_task_status outside a
	//     HandleMessage turn are not gated.
	//   - `error` is allowed in all cases: it's the legitimate abort path
	//     for "I can't complete this and have nothing useful to say."
	//   - Same-turn ordering: if the model emits set_task_status BEFORE its
	//     send tool in a single turn, the gate fires once and the next turn
	//     re-issues the status update successfully. One wasted turn at most.
	if services.HasSendTracker(ctx) && services.DeliveryCount(ctx) == 0 {
		if params.Status == string(services.MemoryStatus_Waiting) || params.Status == string(services.MemoryStatus_Complete) {
			return "", fmt.Errorf(
				"cannot set status to %q before delivering a message — no send "+
					"tool has been called this turn. Your assistant text is not "+
					"visible to the recipient; the only way to communicate is to "+
					"call report_strict_tool (or report_tool / message_tool, "+
					"whichever your toolset provides). Send the message first, "+
					"then update the status. If you genuinely have nothing to "+
					"deliver and want to abort, set status to %q instead.",
				params.Status, services.MemoryStatus_Error,
			)
		}
	}

	messageId := services.EnsureAngleBrackets(params.MessageId)
	err := services.SetMemoryStatus(ctx, services.MemoryStatus(params.Status), messageId)
	if err != nil {
		log.Printf("error setting memory status: %s", err)
		return "", err
	}

	// Arm the stuck-task watchdog when entering "waiting". A coordinator that
	// delegates and waits has no other deadline — if the delegate never
	// replies, the thread hangs forever (the failure mode that black-holed the
	// JFE flag task). The watchdog re-wakes this agent after the configured
	// deadline if the thread is still waiting. Use the trusted inbound message
	// from ctx, never LLM input, so the replay targets the real message. Gated
	// on a real send this turn (DeliveryCount) so we don't arm a watchdog for a
	// turn that never actually delivered anything.
	if params.Status == string(services.MemoryStatus_Waiting) &&
		config.WaitingWatchdogMinutes > 0 &&
		services.DeliveryCount(ctx) > 0 {
		if src := services.SourceEmail(ctx); src != nil && src.MessageId != "" {
			if armErr := services.ArmWaitingWatchdog(ctx, src.MessageId, src.GetThreadRoot(), config.WaitingWatchdogMinutes); armErr != nil {
				log.Printf("warning: could not arm waiting watchdog: %s", armErr)
			}
		}
	}

	return "Status Updated", nil
}
