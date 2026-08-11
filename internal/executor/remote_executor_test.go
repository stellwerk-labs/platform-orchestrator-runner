package executor

import (
	"context"
	"testing"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"
	k8smock "github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob/mocks"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createJobCommand() hmessaging.CreateJobCommand {
	return hmessaging.CreateJobCommand{
		JobID: "job-id", Namespace: "default",
		Configuration: map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"restartPolicy": "Never",
					"containers": []interface{}{map[string]interface{}{
						"name": "runner", "image": "ghcr.io/stellwerk-labs/platform-orchestrator-runner:latest",
					}},
				},
			},
		},
	}
}

func remoteModeConfig() *config.RemoteModeConfiguration {
	return &config.RemoteModeConfiguration{
		OrgID: "test-org", RunnerId: "remote-runner",
		Gateway: config.GatewayClientConfiguration{URL: "https://gateway.example.com/runner-gateway"},
	}
}

func TestHandleCreateJobSuccess(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	jobs.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, job *batchv1.Job) (*batchv1.Job, error) {
			require.Equal(t, "job-id", job.Name)
			require.Equal(t, "default", job.Namespace)
			require.Len(t, job.Spec.Template.Spec.Containers, 1)
			require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "RUNNER_GATEWAY_URL", Value: "https://gateway.example.com/runner-gateway"})
			require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "ORG_ID", Value: "test-org"})
			require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "RUNNER_ID", Value: "remote-runner"})
			return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job.Name}}, nil
		},
	)
	require.NoError(t, handleCreateJob(t.Context(), createJobCommand(), jobs, remoteModeConfig()))
}

func TestHandleCreateJobAlreadyExistsIsIdempotent(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	jobs.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).Return(
		nil, k8serrors.NewAlreadyExists(batchv1.Resource("jobs"), "job-id"),
	)
	require.ErrorIs(t, handleCreateJob(t.Context(), createJobCommand(), jobs, remoteModeConfig()), ErrConflict)
}

func TestHandleCreateJobReturnsKubernetesError(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	jobs.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).Return(
		nil, k8serrors.NewForbidden(batchv1.Resource("jobs"), "job-id", errors.New("denied")),
	)
	err := handleCreateJob(t.Context(), createJobCommand(), jobs, remoteModeConfig())
	require.ErrorContains(t, err, "failed to create job")
	require.ErrorContains(t, err, "denied")
}

func TestHandleCreateJobRejectsIncompleteCommand(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	require.ErrorContains(t, handleCreateJob(t.Context(), hmessaging.CreateJobCommand{}, jobs, remoteModeConfig()), "requires job_id")
}

func TestPermanentJobCreationErrors(t *testing.T) {
	for name, err := range map[string]error{
		"bad request":  k8serrors.NewBadRequest("bad job"),
		"forbidden":    k8serrors.NewForbidden(batchv1.Resource("jobs"), "job-id", errors.New("denied")),
		"invalid":      k8serrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "job-id", nil),
		"not found":    k8serrors.NewNotFound(batchv1.Resource("namespaces"), "missing"),
		"unauthorized": k8serrors.NewUnauthorized("authentication failed"),
	} {
		t.Run(name, func(t *testing.T) {
			require.True(t, isPermanentJobCreationError(errors.Wrap(err, "failed to create job")))
		})
	}
	require.False(t, isPermanentJobCreationError(errors.New("connection reset")))
}

func TestJobSchedulingStateReportsJobWarningWithoutPod(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	jobs.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&batchv1.JobStatus{}, nil)
	jobs.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrNotFound)
	jobs.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "job-id").Return(
		[]string{"FailedCreate - serviceaccount missing not found"}, nil,
	)

	message, done, err := jobSchedulingState(t.Context(), jobs, "default", "job-id")
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, "FailedCreate - serviceaccount missing not found", message)
}

func TestJobSchedulingStateStopsOncePodIsRunning(t *testing.T) {
	jobs := k8smock.NewMockK8sJobsClientInterface(gomock.NewController(t))
	jobs.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&batchv1.JobStatus{}, nil)
	jobs.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "job-id-pod"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}, nil)

	message, done, err := jobSchedulingState(t.Context(), jobs, "default", "job-id")
	require.NoError(t, err)
	require.True(t, done)
	require.Empty(t, message)
}
