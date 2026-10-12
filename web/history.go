package web

import (
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/temporal-sa/temporal-demo-expense-approval/expense"
)

// The UI is built from the Workflow's Event History and DescribeWorkflowExecution
// rather than a Query. Both are served by Temporal itself, so every screen keeps
// working while no worker is running, which is exactly when the demo is most
// interesting.

// Step is one row of the journey shown for an expense.
type Step struct {
	Key     string     `json:"key"` // matches a code anchor in the UI
	Label   string     `json:"label"`
	Detail  string     `json:"detail,omitempty"`
	State   string     `json:"state"` // active | waiting | retrying | done | failed
	At      time.Time  `json:"at"`
	Until   *time.Time `json:"until,omitempty"` // timer deadline, for a countdown
	Attempt int32      `json:"attempt,omitempty"`
}

// Expense is the UI's view of one ExpenseApproval execution.
type Expense struct {
	ID          string    `json:"id"`
	WorkflowID  string    `json:"workflowId"`
	RunID       string    `json:"runId"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	AmountCents int64     `json:"amountCents"`
	Employee    string    `json:"employee"`
	SubmittedAt time.Time `json:"submittedAt"`

	Running bool   `json:"running"`
	Label   string `json:"label"` // short status for lists
	Tone    string `json:"tone"`  // good | progress | waiting | idle | error

	// AwaitingDecision is true while an approver can still decide. CanDecide
	// lists who; DecisionRecorded is set once a decision Signal is in history.
	AwaitingDecision bool                   `json:"awaitingDecision"`
	CanDecide        []string               `json:"canDecide,omitempty"`
	DecisionRecorded *expense.Decision      `json:"decisionRecorded,omitempty"`
	Result           *expense.ExpenseResult `json:"result,omitempty"`

	// WaitingForWorkerSince is set when Temporal has work ready for this
	// execution that no worker has picked up for a few seconds.
	WaitingForWorkerSince *time.Time `json:"waitingForWorkerSince,omitempty"`

	Steps []Step `json:"steps"`
}

var activityLabels = map[string]struct{ key, label string }{
	"CheckPolicy":      {"policy", "Check expense policy"},
	"NotifyApprover":   {"notify-approver", "Notify approver"},
	"PayReimbursement": {"pay", "Pay reimbursement"},
	"NotifyEmployee":   {"notify-employee", "Notify employee"},
}

// workerGrace is how long ready work may sit before the UI says no worker has
// picked it up. Workers normally start a task within milliseconds.
const workerGrace = 3 * time.Second

func buildExpense(id string, events []*historypb.HistoryEvent, desc *workflowservice.DescribeWorkflowExecutionResponse, now time.Time) (*Expense, error) {
	dc := converter.GetDefaultDataConverter()
	e := &Expense{ID: id, Running: true}
	var req expense.ExpenseRequest

	activitySteps := map[int64]int{} // scheduled event ID -> index in e.Steps
	timerSteps := map[string]int{}   // timer ID -> index in e.Steps
	timersStarted, escalated, waitOver := 0, false, false

	// Work becomes ready for a worker with the first event after the last
	// completed workflow task: a Signal, a fired timer, a finished Activity.
	var unprocessedSince *time.Time

	// The Go SDK doesn't cancel AwaitWithTimeout's timer when the condition is
	// met; the timer just stops mattering. A wait ends once the Workflow has
	// processed the decision Signal.
	closeWaits := func(at time.Time) {
		for _, i := range timerSteps {
			if s := &e.Steps[i]; s.State == "waiting" {
				s.State, s.At, s.Until, s.Detail = "done", at, nil, "Decision arrived before the deadline"
			}
		}
	}

	for _, ev := range events {
		at := ev.GetEventTime().AsTime()
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED:
			unprocessedSince = nil
			if e.DecisionRecorded != nil {
				closeWaits(at)
			}
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED,
			enumspb.EVENT_TYPE_TIMER_FIRED, enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED,
			enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED, enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			if unprocessedSince == nil {
				unprocessedSince = &at
			}
		}
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			attrs := ev.GetWorkflowExecutionStartedEventAttributes()
			if err := dc.FromPayloads(attrs.GetInput(), &req); err != nil {
				return nil, fmt.Errorf("decode input: %w", err)
			}
			e.Description, e.Category, e.AmountCents, e.Employee, e.SubmittedAt = req.Description, req.Category, req.AmountCents, req.Employee, at
			e.Steps = append(e.Steps, Step{Key: "submit", Label: "Submitted by " + req.Employee, Detail: expense.FormatCents(req.AmountCents) + " · " + req.Category, State: "done", At: at})

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			attrs := ev.GetActivityTaskScheduledEventAttributes()
			name := attrs.GetActivityType().GetName()
			meta := activityLabels[name]
			step := Step{Key: meta.key, Label: meta.label, State: "active", At: at}
			if name == "NotifyApprover" {
				var notice expense.ApprovalNotice
				if dc.FromPayloads(attrs.GetInput(), &notice) == nil {
					step.Label = "Notify " + notice.To
					if notice.Escalated {
						escalated = true
						step.Key, step.Label = "escalate", "Escalate to "+notice.To
					}
				}
			}
			activitySteps[ev.GetEventId()] = len(e.Steps)
			e.Steps = append(e.Steps, step)

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			attrs := ev.GetActivityTaskStartedEventAttributes()
			if i, ok := activitySteps[attrs.GetScheduledEventId()]; ok {
				e.Steps[i].Attempt = attrs.GetAttempt()
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			attrs := ev.GetActivityTaskCompletedEventAttributes()
			if i, ok := activitySteps[attrs.GetScheduledEventId()]; ok {
				s := &e.Steps[i]
				s.State, s.At = "done", at
				s.Detail = activityResultDetail(dc, s.Key, attrs)
				if s.Attempt > 1 {
					s.Detail += fmt.Sprintf(" · succeeded on attempt %d", s.Attempt)
				}
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED, enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			var scheduledID int64
			if a := ev.GetActivityTaskFailedEventAttributes(); a != nil {
				scheduledID = a.GetScheduledEventId()
			} else {
				scheduledID = ev.GetActivityTaskTimedOutEventAttributes().GetScheduledEventId()
			}
			if i, ok := activitySteps[scheduledID]; ok {
				e.Steps[i].State, e.Steps[i].At = "failed", at
			}

		case enumspb.EVENT_TYPE_TIMER_STARTED:
			attrs := ev.GetTimerStartedEventAttributes()
			timersStarted++
			until := at.Add(attrs.GetStartToFireTimeout().AsDuration())
			step := Step{Key: "wait", Label: "Waiting for " + req.Approver, Detail: "Durable timer: escalates if no decision", State: "waiting", At: at, Until: &until}
			if timersStarted > 1 {
				step.Key, step.Label, step.Detail = "wait-escalated", "Waiting for "+req.Approver+" or "+req.EscalationApprover, "Durable timer: expires if no decision"
			}
			timerSteps[attrs.GetTimerId()] = len(e.Steps)
			e.Steps = append(e.Steps, step)

		case enumspb.EVENT_TYPE_TIMER_FIRED:
			if e.DecisionRecorded != nil {
				closeWaits(at) // the Signal is earlier in history, so the Workflow sees it first
				continue
			}
			if i, ok := timerSteps[ev.GetTimerFiredEventAttributes().GetTimerId()]; ok {
				s := &e.Steps[i]
				s.State, s.At, s.Until = "done", at, nil
				s.Detail = "No decision in time"
			}
			waitOver = timersStarted > 1

		case enumspb.EVENT_TYPE_TIMER_CANCELED:
			if i, ok := timerSteps[ev.GetTimerCanceledEventAttributes().GetTimerId()]; ok {
				s := &e.Steps[i]
				s.State, s.At, s.Until = "done", at, nil
				s.Detail = "Decision arrived; timer canceled"
			}
			waitOver = true

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			attrs := ev.GetWorkflowExecutionSignaledEventAttributes()
			var d expense.Decision
			if attrs.GetSignalName() != expense.DecisionSignal || dc.FromPayloads(attrs.GetInput(), &d) != nil {
				continue
			}
			verb := "Rejected"
			if d.Approved {
				verb = "Approved"
			}
			if e.DecisionRecorded == nil {
				e.DecisionRecorded = &d
				for _, i := range timerSteps {
					if s := &e.Steps[i]; s.State == "waiting" {
						s.Until, s.Detail = nil, "Decision received; the Workflow acts on it next"
					}
				}
			}
			e.Steps = append(e.Steps, Step{Key: "decision", Label: verb + " by " + d.By, Detail: quoteOr(d.Comment, "Signal recorded in the Event History"), State: "done", At: at})

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
			var result expense.ExpenseResult
			if err := dc.FromPayloads(ev.GetWorkflowExecutionCompletedEventAttributes().GetResult(), &result); err != nil {
				return nil, fmt.Errorf("decode result: %w", err)
			}
			e.Result, e.Running = &result, false
			e.Steps = append(e.Steps, Step{Key: "complete", Label: outcomeLabel(result.Outcome), Detail: result.Reason, State: "done", At: at})

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED,
			enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
			e.Running = false
			e.Steps = append(e.Steps, Step{Key: "complete", Label: "Stopped", Detail: "The execution was " + closedVerb(ev.GetEventType()), State: "failed", At: at})
		}
	}

	if e.Running && desc != nil {
		applyPending(e, desc, unprocessedSince, now)
	}

	notified := false
	for _, s := range e.Steps {
		notified = notified || s.Key == "notify-approver"
	}
	e.AwaitingDecision = e.Running && notified && !waitOver && e.DecisionRecorded == nil
	if e.AwaitingDecision {
		e.CanDecide = []string{req.Approver}
		if escalated {
			e.CanDecide = append(e.CanDecide, req.EscalationApprover)
		}
	}
	e.Label, e.Tone = statusLabel(e, req, escalated)
	return e, nil
}

// applyPending adds what history can't show yet: retries in progress, and work
// that is ready but has no worker to run it.
func applyPending(e *Expense, desc *workflowservice.DescribeWorkflowExecutionResponse, unprocessedSince *time.Time, now time.Time) {
	var since *time.Time
	noteWaiting := func(t time.Time) {
		if now.Sub(t) > workerGrace && (since == nil || t.Before(*since)) {
			since = &t
		}
	}

	// A workflow task first goes to the last worker's sticky queue and is
	// rescheduled if that worker is gone, so its scheduled time can reset.
	// The first unprocessed event says how long the work has really been ready.
	if wt := desc.GetPendingWorkflowTask(); wt != nil && wt.GetState() == enumspb.PENDING_WORKFLOW_TASK_STATE_SCHEDULED {
		ready := wt.GetScheduledTime().AsTime()
		if unprocessedSince != nil && unprocessedSince.Before(ready) {
			ready = *unprocessedSince
		}
		noteWaiting(ready)
	}

	for _, pa := range desc.GetPendingActivities() {
		meta := activityLabels[pa.GetActivityType().GetName()]
		var s *Step
		for i := len(e.Steps) - 1; i >= 0; i-- {
			if e.Steps[i].State == "active" && (e.Steps[i].Key == meta.key || meta.key == "notify-approver" && e.Steps[i].Key == "escalate") {
				s = &e.Steps[i]
				break
			}
		}
		if s == nil {
			continue
		}
		s.Attempt = pa.GetAttempt()
		next := pa.GetNextAttemptScheduleTime()
		if pa.GetAttempt() > 1 {
			s.State = "retrying"
			s.Detail = fmt.Sprintf("Attempt %d failed: %s. Temporal will retry it", pa.GetAttempt()-1, pa.GetLastFailure().GetMessage())
			if next != nil && next.AsTime().After(now) {
				t := next.AsTime()
				s.Until = &t
			}
		}
		// A scheduled attempt is ready once any retry backoff has elapsed.
		if pa.GetState() == enumspb.PENDING_ACTIVITY_STATE_SCHEDULED {
			ready := pa.GetScheduledTime().AsTime()
			if next != nil && next.AsTime().After(ready) {
				ready = next.AsTime()
			}
			noteWaiting(ready)
		}
	}
	e.WaitingForWorkerSince = since
}

func activityResultDetail(dc converter.DataConverter, key string, attrs *historypb.ActivityTaskCompletedEventAttributes) string {
	switch key {
	case "policy":
		var p expense.PolicyResult
		if dc.FromPayloads(attrs.GetResult(), &p) == nil {
			return p.Reason
		}
	default:
		var s string
		if dc.FromPayloads(attrs.GetResult(), &s) == nil {
			if key == "pay" {
				return "Payment reference " + s
			}
			return s
		}
	}
	return ""
}

func statusLabel(e *Expense, req expense.ExpenseRequest, escalated bool) (string, string) {
	if !e.Running {
		if e.Result == nil {
			return "Stopped", "error"
		}
		switch e.Result.Outcome {
		case expense.OutcomeApproved, expense.OutcomeAutoApproved:
			return "Paid", "good"
		case expense.OutcomeExpired:
			return "Expired", "idle"
		case expense.OutcomePolicyRejected:
			return "Over policy limit", "idle"
		default:
			return "Rejected", "idle"
		}
	}
	switch {
	case e.WaitingForWorkerSince != nil:
		return "Waiting for a worker", "waiting"
	case e.DecisionRecorded != nil && hasWaiting(e):
		return "Decision recorded", "progress"
	case e.AwaitingDecision && escalated:
		return "Escalated to " + req.EscalationApprover, "waiting"
	case e.AwaitingDecision:
		return "Awaiting " + req.Approver, "waiting"
	}
	for i := len(e.Steps) - 1; i >= 0; i-- {
		if s := e.Steps[i]; s.State == "active" || s.State == "retrying" {
			return s.Label, "progress"
		}
	}
	return "In progress", "progress"
}

func outcomeLabel(o expense.Outcome) string {
	switch o {
	case expense.OutcomeAutoApproved:
		return "Auto-approved and paid"
	case expense.OutcomeApproved:
		return "Approved and paid"
	case expense.OutcomeRejected:
		return "Rejected"
	case expense.OutcomePolicyRejected:
		return "Rejected by policy"
	case expense.OutcomeExpired:
		return "Expired"
	}
	return string(o)
}

func closedVerb(t enumspb.EventType) string {
	switch t {
	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
		return "terminated"
	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
		return "canceled"
	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
		return "timed out"
	}
	return "failed"
}

func quoteOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return "“" + s + "”"
}

func hasWaiting(e *Expense) bool {
	for _, s := range e.Steps {
		if s.State == "waiting" {
			return true
		}
	}
	return false
}
