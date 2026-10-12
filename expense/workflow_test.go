package expense

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func newRequest(amountCents int64) ExpenseRequest {
	return ExpenseRequest{
		ExpenseID:          "t1",
		Employee:           "Alex Rivera",
		Description:        "Conference ticket",
		Category:           "Travel",
		AmountCents:        amountCents,
		Approver:           "Sam Chen",
		EscalationApprover: "Dana Ortiz",
		EscalateAfter:      time.Minute,
		ExpireAfter:        10 * time.Minute,
	}
}

func run(t *testing.T, req ExpenseRequest, setup func(env *testsuite.TestWorkflowEnvironment)) ExpenseResult {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})
	if setup != nil {
		setup(env)
	}
	env.ExecuteWorkflow(ExpenseApproval, req)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result ExpenseResult
	require.NoError(t, env.GetWorkflowResult(&result))
	return result
}

func decide(env *testsuite.TestWorkflowEnvironment, after time.Duration, d Decision) {
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(DecisionSignal, d) }, after)
}

func TestSmallExpenseIsAutoApprovedAndPaid(t *testing.T) {
	result := run(t, newRequest(18_50), nil)
	require.Equal(t, OutcomeAutoApproved, result.Outcome)
	require.Equal(t, "PAY-T1", result.PaymentRef)
}

func TestExpenseOverLimitIsRejectedByPolicy(t *testing.T) {
	result := run(t, newRequest(7_200_00), nil)
	require.Equal(t, OutcomePolicyRejected, result.Outcome)
	require.Empty(t, result.PaymentRef)
}

func TestManagerApprovalIsPaid(t *testing.T) {
	result := run(t, newRequest(649_00), func(env *testsuite.TestWorkflowEnvironment) {
		decide(env, 10*time.Second, Decision{Approved: true, By: "Sam Chen", Comment: "Enjoy the conference"})
		// A second click must not change the first decision.
		decide(env, 11*time.Second, Decision{Approved: false, By: "Sam Chen"})
	})
	require.Equal(t, OutcomeApproved, result.Outcome)
	require.Equal(t, "Enjoy the conference", result.Reason)
	require.Equal(t, "PAY-T1", result.PaymentRef)
}

func TestManagerRejectionIsNotPaid(t *testing.T) {
	result := run(t, newRequest(649_00), func(env *testsuite.TestWorkflowEnvironment) {
		decide(env, 10*time.Second, Decision{Approved: false, By: "Sam Chen", Comment: "Not in budget"})
	})
	require.Equal(t, OutcomeRejected, result.Outcome)
	require.Empty(t, result.PaymentRef)
}

func TestEscalationApproverCanDecideOnlyAfterEscalation(t *testing.T) {
	result := run(t, newRequest(649_00), func(env *testsuite.TestWorkflowEnvironment) {
		decide(env, 10*time.Second, Decision{Approved: false, By: "Dana Ortiz"}) // ignored: not escalated yet
		decide(env, 2*time.Minute, Decision{Approved: true, By: "Dana Ortiz"})
		env.RegisterDelayedCallback(func() {
			v, err := env.QueryWorkflow(StatusQuery)
			require.NoError(t, err)
			var s Status
			require.NoError(t, v.Get(&s))
			require.Equal(t, StageEscalated, s.Stage)
		}, 90*time.Second)
	})
	require.Equal(t, OutcomeApproved, result.Outcome)
	require.Equal(t, "Dana Ortiz", result.Decision.By)
}

func TestRequestExpiresWithoutDecision(t *testing.T) {
	result := run(t, newRequest(649_00), nil)
	require.Equal(t, OutcomeExpired, result.Outcome)
	require.Nil(t, result.Decision)
}

func TestPaymentOutageIsRetried(t *testing.T) {
	req := newRequest(18_50)
	req.Demo.PaymentFailures = 2
	result := run(t, req, nil)
	require.Equal(t, OutcomeAutoApproved, result.Outcome)
	require.Equal(t, "PAY-T1", result.PaymentRef)
}
