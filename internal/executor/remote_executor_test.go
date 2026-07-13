package executor

import (
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"
	k8smock "github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHandleJobMessage_create_job_success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	assert.NoError(t, msg.FromRemoteRunnerMessageCreateJob(platformorchestratorapi.RemoteRunnerMessageCreateJob{
		JobId:           "job-id",
		Action:          platformorchestratorapi.CreateJob,
		Namespace:       "default",
		DeploymentToken: "token",
		Configuration: map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "runner",
							"image": "ghcr.io/stellwerk-labs/platform-orchestrator-runner:latest",
						},
					},
				},
			},
		}}))
	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).Return(&v1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job-id"}}, nil)
	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_create_job_already_exists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	assert.NoError(t, msg.FromRemoteRunnerMessageCreateJob(platformorchestratorapi.RemoteRunnerMessageCreateJob{
		JobId:           "job-id",
		Action:          platformorchestratorapi.CreateJob,
		Namespace:       "default",
		DeploymentToken: "token",
		Configuration: map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "runner",
							"image": "ghcr.io/stellwerk-labs/platform-orchestrator-runner:latest",
						},
					},
				},
			},
		}}))
	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).Return(nil, k8serrors.NewAlreadyExists(v1.Resource("jobs"), "job-id"))
	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.ErrorIs(t, ErrConflict, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_create_job_error(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	assert.NoError(t, msg.FromRemoteRunnerMessageCreateJob(platformorchestratorapi.RemoteRunnerMessageCreateJob{
		JobId:           "job-id",
		Action:          platformorchestratorapi.CreateJob,
		Namespace:       "default",
		DeploymentToken: "token",
		Configuration: map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "runner",
							"image": "ghcr.io/stellwerk-labs/platform-orchestrator-orchestrator-platform-orchestrator:latest",
						},
					},
				},
			},
		}}))
	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CreateJob(gomock.Any(), "default", gomock.Any()).Return(nil, k8serrors.NewForbidden(v1.Resource("jobs"), "job-id", errors.New("User \"test-user\" cannot create resource \"jobs\" in API group \"batch\" in the namespace \"default\"")))
	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "failed to create job: jobs.batch \"job-id\" is forbidden: User \"test-user\" cannot create resource \"jobs\" in API group \"batch\" in the namespace \"default\"")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 1,
		Failed:    0,
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_failed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 0,
		Failed:    1,
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_active_and_ready(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(1)),
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_active_but_pod_not_ready_pending(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(&corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_active_but_pod_not_ready_unknown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(&corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodUnknown,
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_not_found_not_expired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrNotFound)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_not_found_expired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrNotFound)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job not found after the expiration time")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_stuck_expired_with_warnings(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 0,
		Failed:    0,
	}, nil)
	k8sClientMock.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "job-id").Return([]string{
		"FailedScheduling - insufficient resources",
		"FailedMount - volume not found",
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job seems to be stuck: FailedScheduling - insufficient resources/nFailedMount - volume not found")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_stuck_expired_with_no_warnings(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 0,
		Failed:    0,
	}, nil)
	k8sClientMock.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "job-id").Return([]string{}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job seems to be stuck: job has not started and there are no warning events for the job or its pod")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_stuck_expired_pod_not_ready(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(&corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
	}, nil)
	k8sClientMock.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "test-pod").Return([]string{
		"FailedPullImage - image not found",
	}, nil)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job seems to be stuck: FailedPullImage - image not found")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_check_job_error(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(nil, errors.New("k8s error"))

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "failed to check job status: k8s error")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_check_pod_error(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(nil, errors.New("pod k8s error"))

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "failed to check pod job status: pod k8s error")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_pod_not_found_not_ready(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrNotFound)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_forbidden(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(nil, k8serrors.NewForbidden(v1.Resource("jobs"), "job-id", errors.New("User \"test-user\" cannot get resource \"jobs\" in API group \"batch\" in the namespace \"default\"")))

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "failed to check job status: jobs.batch \"job-id\" is forbidden: User \"test-user\" cannot get resource \"jobs\" in API group \"batch\" in the namespace \"default\"")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_pod_forbidden(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(5 * time.Minute)
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    1,
		Succeeded: 0,
		Failed:    0,
		Ready:     ref.Ref(int32(0)),
	}, nil)
	k8sClientMock.EXPECT().GetPodJob(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrK8sActionForbidden)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.NoError(t, err)
	assert.Empty(t, deploymentId)
	assert.Empty(t, token)
}

func TestHandleJobMessage_check_job_status_events_forbidden_expired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 0,
		Failed:    0,
	}, nil)
	k8sClientMock.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "job-id").Return(nil, kubernetesjob.ErrK8sActionForbidden)

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job seems to be stuck: job has not started and the runner configuration does not allow to read job events and pods in the target namespace, please check the runner configuration")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}

func TestHandleJobMessage_check_job_status_events_error_expired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	msg := new(platformorchestratorapi.RemoteRunnerMessage)
	expirationTime := time.Now().Add(-1 * time.Minute) // expired
	assert.NoError(t, msg.FromRemoteRunnerMessageCheckJobStatus(platformorchestratorapi.RemoteRunnerMessageCheckJobStatus{
		JobId:           "job-id",
		Namespace:       "default",
		DeploymentToken: "token",
		ExpiresAt:       expirationTime,
	}))

	k8sClientMock := k8smock.NewMockK8sJobsClientInterface(ctrl)
	k8sClientMock.EXPECT().CheckJobStatus(gomock.Any(), "default", "job-id").Return(&v1.JobStatus{
		Active:    0,
		Succeeded: 0,
		Failed:    0,
	}, nil)
	k8sClientMock.EXPECT().GetObjectWarningEvents(gomock.Any(), "default", "job-id").Return(nil, errors.New("k8s error"))

	deploymentId, token, err := handleJobMessage(t.Context(), *msg, k8sClientMock)
	assert.EqualError(t, err, "job seems to be stuck: job has not started and failed to get job warning events: k8s error")
	assert.Equal(t, "job-id", deploymentId)
	assert.Equal(t, "token", token)
}
