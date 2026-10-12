// Command worker runs the ExpenseApproval Workflow and its Activities.
package main

import (
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/temporal-sa/temporal-demo-expense-approval/expense"
	"github.com/temporal-sa/temporal-demo-expense-approval/internal/config"
)

func main() {
	logger := config.Logger()
	opts, err := config.ClientOptions(logger)
	if err != nil {
		logger.Error("invalid Temporal client configuration", "error", err)
		os.Exit(1)
	}
	c, err := client.Dial(opts)
	if err != nil {
		logger.Error("cannot connect to Temporal", "address", opts.HostPort, "error", err)
		os.Exit(1)
	}
	defer c.Close()

	taskQueue := config.TaskQueue()
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(expense.ExpenseApproval)
	w.RegisterActivity(&expense.Activities{})

	logger.Info("worker started", "address", opts.HostPort, "namespace", opts.Namespace, "taskQueue", taskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
