package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"

	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var ErrConflict = errors.New("Message already handled")

// ExecuteRemoteMode executes the remote mode with long polling and k8s job triggering
func ExecuteRemoteMode(ctx context.Context, cfg *config.RemoteModeConfiguration, apiClient *platformorchestratorapi.ClientWithResponses, programLevel *slog.LevelVar) error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})).
		With(slog.String("runnerId", cfg.RunnerId)))

	signedJwtToken, err := signJWT([]byte(cfg.PrivateKey), cfg.OrgID, cfg.RunnerId)
	if err != nil {
		return errors.Wrap(err, "failed to sign JWT token")
	}

	var k8sClient *kubernetes.Clientset
	if config, err := rest.InClusterConfig(); err != nil {
		return errors.Wrap(err, "failed to create in-cluster config")
	} else {
		if k8sClient, err = kubernetes.NewForConfig(config); err != nil {
			return errors.Wrap(err, "failed to create cluster client")
		}
	}

	var message platformorchestratorapi.RemoteRunnerMessage
	for {
		if res, err := apiClient.WaitForRemoteRunnerMessagesWithResponse(ctx, cfg.OrgID, cfg.RunnerId, func(ctx context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "JWT "+signedJwtToken)
			return nil
		}); err != nil {
			return errors.Wrap(err, "failed to connect to api to wait for remote runner messages")
		} else if res.StatusCode() == http.StatusOK {
			message = *res.JSON200
			if deploymentId, token, err := handleJobMessage(ctx, message, kubernetesjob.NewK8sJobsClient(k8sClient)); err != nil && !errors.Is(err, ErrConflict) {
				slog.ErrorContext(ctx, "failed to handle job message", "err", err)
				if deploymentId != "" && token != "" {
					if err := utils.SendResultsToApi(ctx, apiClient, cfg.OrgID, deploymentId, token, platformorchestratorapi.DeploymentResultsUpdateBody{
						Status: platformorchestratorapi.Failure,
						Error: &platformorchestratorapi.Error{
							Error:   "REMOTE_RUNNER",
							Message: err.Error(),
						},
					}); err != nil {
						slog.ErrorContext(ctx, "[PLATFORM_ORCHESTRATOR]update-results", "err", err)
					}
				}
			}
			continue
		} else if res.StatusCode() == http.StatusNoContent {
			slog.DebugContext(ctx, "No messages received, waiting for next message")
			continue
		} else {
			// TODO: Instead of simply returning an error, we should send a failure message back to the api
			return errors.Errorf("unexpected status code %d when waiting for messages for remote runner: %s", res.StatusCode(), string(res.Body))
		}
	}
}

func handleJobMessage(ctx context.Context, message platformorchestratorapi.RemoteRunnerMessage, k8sClient kubernetesjob.K8sJobsClientInterface) (string, string, error) {
	messageByAction, _ := message.ValueByDiscriminator()
	switch typed := messageByAction.(type) {
	case platformorchestratorapi.RemoteRunnerMessageCreateJob:
		slog.InfoContext(ctx, "Received message to create a job", "jobId", typed.JobId, "namespace", typed.Namespace)
		jobSpecJson, _ := json.Marshal(typed.Configuration)
		var jobSpec v1.JobSpec
		if err := json.Unmarshal(jobSpecJson, &jobSpec); err != nil {
			return "", "", errors.Wrap(err, "failed to unmarshal job spec")
		}
		if _, err := k8sClient.CreateJob(ctx, typed.Namespace, &v1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      typed.JobId,
				Namespace: typed.Namespace,
			},
			Spec: jobSpec,
		}); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.InfoContext(ctx, "Job already exists, skipping creation", "jobId", typed.JobId, "namespace", typed.Namespace)
				return "", "", ErrConflict
			}
			return typed.JobId, typed.DeploymentToken, errors.Wrap(err, "failed to create job")
		} else {
			slog.InfoContext(ctx, "Job created successfully", "jobId", typed.JobId, "namespace", typed.Namespace)
			return typed.JobId, typed.DeploymentToken, nil
		}

	case platformorchestratorapi.RemoteRunnerMessageCheckJobStatus:
		slog.InfoContext(ctx, "Received message to get job status", "jobId", typed.JobId, "namespace", typed.Namespace, "expiresAt", typed.ExpiresAt)
		jobStatus, err := k8sClient.CheckJobStatus(ctx, typed.Namespace, typed.JobId)
		if err != nil {
			if errors.Is(err, kubernetesjob.ErrNotFound) {
				if typed.ExpiresAt.Before(time.Now()) {
					return typed.JobId, typed.DeploymentToken, errors.New("job not found after the expiration time")
				} else {
					return "", "", nil
				}
			}
			return typed.JobId, typed.DeploymentToken, errors.Wrap(err, "failed to check job status")
		}

		if jobStatus.Failed > 0 || jobStatus.Succeeded > 0 {
			slog.InfoContext(ctx, "Job has completed", "jobId", typed.JobId, "namespace", typed.Namespace, "status", jobStatus)
			return "", "", nil
		}

		var podNotReady bool
		var objectToFetchEventsAbout = typed.JobId
		if jobStatus.Active > 0 && ref.DeRefOr(jobStatus.Ready, 0) == 0 {
			if podJob, err := k8sClient.GetPodJob(ctx, typed.Namespace, typed.JobId); err != nil {
				if errors.Is(err, kubernetesjob.ErrNotFound) {
					podNotReady = true
				} else if errors.Is(err, kubernetesjob.ErrK8sActionForbidden) {
					slog.InfoContext(ctx, "forbidden to get pod info", "jobId", typed.JobId, "namespace", typed.Namespace)
					return "", "", nil
				} else {
					return typed.JobId, typed.DeploymentToken, errors.Wrap(err, "failed to check pod job status")
				}
			} else if podJob != nil && (podJob.Status.Phase == corev1.PodPending || podJob.Status.Phase == corev1.PodUnknown) {
				podNotReady = true
				objectToFetchEventsAbout = podJob.Name
			}
		}

		if (jobStatus.Active == 0 && jobStatus.Failed == 0 && jobStatus.Succeeded == 0) || podNotReady {
			// Job is not found or not started yet
			// we must introduce a delay before considering it stuck as we experienced some system can take long time to schedule the job
			if typed.ExpiresAt.Before(time.Now()) {
				var message string
				// Job or pod has not started for a long time, we should consider it stuck and parse the reason from the events
				if warningEvents, err := k8sClient.GetObjectWarningEvents(ctx, typed.Namespace, objectToFetchEventsAbout); err != nil {
					if errors.Is(err, kubernetesjob.ErrK8sActionForbidden) {
						message = "job has not started and the runner configuration does not allow to read job events and pods in the target namespace, please check the runner configuration"
					} else {
						message = fmt.Sprintf("job has not started and failed to get job warning events: %v", err)
					}
				} else {
					if len(warningEvents) == 0 {
						message = "job has not started and there are no warning events for the job or its pod"
					} else {
						message = strings.Join(warningEvents, "/n")
					}
				}
				return typed.JobId, typed.DeploymentToken, errors.Errorf("job seems to be stuck: %s", message)
			}
		}
		return "", "", nil
	default:
		return "", "", errors.Errorf("received unknown message type %T", typed)
	}
}
