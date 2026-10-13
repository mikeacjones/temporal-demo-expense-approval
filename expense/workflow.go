package expense

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ExpenseApproval checks an expense against policy, routes it to an approver,
// and waits for a decision for as long as it takes. The wait is a durable
// timer: nothing is held in memory while it waits, so a worker crash or
// redeploy loses neither the request nor the decision.
func ExpenseApproval(ctx workflow.Context, req ExpenseRequest) (ExpenseResult, error) {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		// No maximum attempts: a payment provider that is down for an hour
		// should delay the reimbursement, not lose it.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
		},
	})
	var a *Activities

	status := Status{Stage: StageCheckingPolicy}
	if err := workflow.SetQueryHandler(ctx, StatusQuery, func() (Status, error) {
		return status, nil
	}); err != nil {
		return ExpenseResult{}, err
	}

	// Temporal records every Signal in the Event History, even while no worker
	// is running. The first valid decision wins; anything else is logged and
	// ignored, so a double-click or a late reply can't change the outcome.
	decisions := workflow.GetSignalChannel(ctx, DecisionSignal)
	workflow.Go(ctx, func(ctx workflow.Context) {
		for {
			var d Decision
			decisions.Receive(ctx, &d)
			waiting := status.Stage == StageAwaitingApproval || status.Stage == StageEscalated
			allowed := d.By == req.Approver || (status.Stage == StageEscalated && d.By == req.EscalationApprover)
			if !waiting || status.Decision != nil || !allowed {
				logger.Info("decision ignored", "by", d.By, "stage", status.Stage)
				continue
			}
			status.Decision = &d
		}
	})

	var policy PolicyResult
	if err := workflow.ExecuteActivity(ctx, a.CheckPolicy, req).Get(ctx, &policy); err != nil {
		return ExpenseResult{}, err
	}

	result := ExpenseResult{Reason: policy.Reason}
	switch {
	case policy.Rejected:
		result.Outcome = OutcomePolicyRejected
	case policy.AutoApproved:
		result.Outcome = OutcomeAutoApproved
	default:
		status.Stage = StageAwaitingApproval
		notice := ApprovalNotice{To: req.Approver, Request: req}
		if err := workflow.ExecuteActivity(ctx, a.NotifyApprover, notice).Get(ctx, nil); err != nil {
			return ExpenseResult{}, err
		}

		decided := func() bool { return status.Decision != nil }
		ok, err := awaitDecision(ctx, req.EscalateAfter, decided)
		if err != nil {
			return ExpenseResult{}, err
		}
		if !ok {
			status.Stage = StageEscalated
			notice := ApprovalNotice{To: req.EscalationApprover, Request: req, Escalated: true}
			if err := workflow.ExecuteActivity(ctx, a.NotifyApprover, notice).Get(ctx, nil); err != nil {
				return ExpenseResult{}, err
			}
			ok, err = awaitDecision(ctx, req.ExpireAfter-req.EscalateAfter, decided)
			if err != nil {
				return ExpenseResult{}, err
			}
		}

		result.Decision = status.Decision
		switch {
		case !ok:
			result.Outcome, result.Reason = OutcomeExpired, "No decision before the request expired"
		case status.Decision.Approved:
			result.Outcome, result.Reason = OutcomeApproved, status.Decision.Comment
		default:
			result.Outcome, result.Reason = OutcomeRejected, status.Decision.Comment
		}
	}

	if result.Outcome == OutcomeApproved || result.Outcome == OutcomeAutoApproved {
		status.Stage = StagePaying
		if err := workflow.ExecuteActivity(ctx, a.PayReimbursement, req).Get(ctx, &result.PaymentRef); err != nil {
			return ExpenseResult{}, err
		}
	}

	status.Stage = StageNotifying
	employeeNotice := EmployeeNotice{Request: req, Result: result}
	if err := workflow.ExecuteActivity(ctx, a.NotifyEmployee, employeeNotice).Get(ctx, nil); err != nil {
		return ExpenseResult{}, err
	}

	status.Stage = StageDone
	logger.Info("expense request finished", "outcome", result.Outcome)
	return result, nil
}

// awaitDecision waits until a decision is recorded or the timeout passes, and
// reports whether a decision arrived. The timer runs on its own cancelable
// context, so when the decision wins the timer is canceled and the Event
// History records it, instead of leaving a timer that fires for nothing.
func awaitDecision(ctx workflow.Context, timeout time.Duration, decided func() bool) (bool, error) {
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	timer := workflow.NewTimer(timerCtx, timeout)
	if err := workflow.Await(ctx, func() bool { return decided() || timer.IsReady() }); err != nil {
		return false, err
	}
	return decided(), nil
}
