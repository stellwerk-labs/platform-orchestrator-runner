package rollout

import (
	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

const rolloutQueue = "site-rollouts"

func (e *Engine) initializeQueue(client dbos.Client) error {
	pg := e.config.Postgres
	if _, err := dbos.RegisterQueue(client, rolloutQueue, dbos.WithQueueOnConflict(dbos.QueueConflictNeverUpdate), dbos.WithGlobalConcurrency(pg.Concurrency), dbos.WithWorkerConcurrency(pg.WorkerConcurrency)); err != nil {
		return errStorageUnavailable
	}
	if _, err := dbos.RegisterQueue(client, controlReceiptQueue, dbos.WithQueueOnConflict(dbos.QueueConflictNeverUpdate), dbos.WithGlobalConcurrency(4), dbos.WithWorkerConcurrency(1)); err != nil {
		return errStorageUnavailable
	}
	return e.verifyQueue(client)
}

func (e *Engine) verifyQueue(client dbos.Client) error {
	queue, err := dbos.RetrieveQueue(client, rolloutQueue)
	if err != nil {
		return refused("queue_configuration_refused")
	}
	pg := e.config.Postgres
	global, worker := queue.GetGlobalConcurrency(), queue.GetWorkerConcurrency()
	if queue.GetApplicationName() != "stellwerk-runner-rollouts" || global == nil || worker == nil || *global != pg.Concurrency || *worker != pg.WorkerConcurrency || queue.GetPartitionQueue() || queue.GetRateLimit() != nil || queue.GetPriorityEnabled() {
		return refused("queue_configuration_refused")
	}
	receiptQueue, err := dbos.RetrieveQueue(client, controlReceiptQueue)
	if err != nil {
		return refused("queue_configuration_refused")
	}
	global, worker = receiptQueue.GetGlobalConcurrency(), receiptQueue.GetWorkerConcurrency()
	if receiptQueue.GetApplicationName() != controlApplication || global == nil || worker == nil || *global != 4 || *worker != 1 || receiptQueue.GetPartitionQueue() || receiptQueue.GetRateLimit() != nil || receiptQueue.GetPriorityEnabled() {
		return refused("queue_configuration_refused")
	}
	return nil
}
