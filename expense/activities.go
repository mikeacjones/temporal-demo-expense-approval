package expense

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
)

const (
	autoApproveLimitCents = 50_00
	policyLimitCents      = 5_000_00
)

// Activities are the steps that touch the outside world. In a real system this
// struct would hold clients for the policy service, email, and payments; here
// each step simulates a short call so its progress is visible in the demo.
type Activities struct{}

// CheckPolicy applies company expense policy: small expenses are approved
// automatically, very large ones are refused, and everything else needs a person.
func (a *Activities) CheckPolicy(ctx context.Context, req ExpenseRequest) (PolicyResult, error) {
	if err := simulateCall(ctx, 800*time.Millisecond); err != nil {
		return PolicyResult{}, err
	}

	var result PolicyResult
	switch {
	case req.AmountCents > policyLimitCents:
		result = PolicyResult{Rejected: true, Reason: "Over the " + FormatCents(policyLimitCents) + " per-expense limit"}
	case req.AmountCents <= autoApproveLimitCents:
		result = PolicyResult{AutoApproved: true, Reason: "Within the " + FormatCents(autoApproveLimitCents) + " auto-approval limit"}
	default:
		result = PolicyResult{Reason: "Needs approval from " + req.Approver}
	}
	activity.GetLogger(ctx).Info("policy checked", "expense", req.ExpenseID, "amount", FormatCents(req.AmountCents), "result", result.Reason)
	return result, nil
}

// NotifyApprover asks an approver to review the request. The returned message
// stands in for the email a real system would send.
func (a *Activities) NotifyApprover(ctx context.Context, notice ApprovalNotice) (string, error) {
	if err := simulateCall(ctx, 600*time.Millisecond); err != nil {
		return "", err
	}

	req := notice.Request
	msg := fmt.Sprintf("Emailed %s: please review %q (%s) from %s", notice.To, req.Description, FormatCents(req.AmountCents), req.Employee)
	if notice.Escalated {
		msg = fmt.Sprintf("Escalated to %s: %q (%s) has waited too long for %s", notice.To, req.Description, FormatCents(req.AmountCents), req.Approver)
	}
	activity.GetLogger(ctx).Info("approver notified", "expense", req.ExpenseID, "to", notice.To, "escalated", notice.Escalated)
	return msg, nil
}

// PayReimbursement pays the employee. The Workflow ID is a stable idempotency
// key: a real payment API that receives the same key on every retry pays at
// most once, so Temporal can retry this step safely.
func (a *Activities) PayReimbursement(ctx context.Context, req ExpenseRequest) (string, error) {
	info := activity.GetInfo(ctx)
	if err := simulateCall(ctx, time.Second); err != nil {
		return "", err
	}

	// Demo failure injection: fail the first N attempts so the Retry Policy is visible.
	if int(info.Attempt) <= req.Demo.PaymentFailures {
		return "", errors.New("payment provider unavailable (simulated outage)")
	}

	ref := "PAY-" + strings.ToUpper(req.ExpenseID)
	activity.GetLogger(ctx).Info("reimbursement paid", "expense", req.ExpenseID, "amount", FormatCents(req.AmountCents), "ref", ref, "idempotencyKey", info.WorkflowExecution.ID)
	return ref, nil
}

// NotifyEmployee tells the employee how their request ended.
func (a *Activities) NotifyEmployee(ctx context.Context, notice EmployeeNotice) (string, error) {
	if err := simulateCall(ctx, 500*time.Millisecond); err != nil {
		return "", err
	}

	req, result := notice.Request, notice.Result
	var msg string
	switch result.Outcome {
	case OutcomeApproved, OutcomeAutoApproved:
		msg = fmt.Sprintf("Emailed %s: %s for %q is on its way (%s)", req.Employee, FormatCents(req.AmountCents), req.Description, result.PaymentRef)
	case OutcomeExpired:
		msg = fmt.Sprintf("Emailed %s: %q expired without a decision. Please resubmit", req.Employee, req.Description)
	default:
		msg = fmt.Sprintf("Emailed %s: %q was not approved. %s", req.Employee, req.Description, result.Reason)
	}
	activity.GetLogger(ctx).Info("employee notified", "expense", req.ExpenseID, "outcome", result.Outcome)
	return msg, nil
}

func simulateCall(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FormatCents renders an amount such as 64900 as "$649.00".
func FormatCents(cents int64) string {
	dollars, rem := cents/100, cents%100
	s := fmt.Sprintf("%d", dollars)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return fmt.Sprintf("$%s.%02d", s, rem)
}
