package executor

import (
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

func fromIaCChangesToTFResourceCounts(changes *runner.IaCChanges) platformorchestratorapi.DeploymentTFResourceCounts {
	if changes == nil {
		return platformorchestratorapi.DeploymentTFResourceCounts{}
	} else {
		return platformorchestratorapi.DeploymentTFResourceCounts{
			NumResourcesAdded:   changes.Added,
			NumResourcesChanged: changes.Change,
			NumResourcesRemoved: changes.Remove,
		}
	}
}
