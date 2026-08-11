package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"

	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var ErrConflict = errors.New("Message already handled")

type remoteGateway interface {
	NextCommand(ctx context.Context) (gatewayapi.CommandResponse, error)
	Acknowledge(ctx context.Context, commandID, receipt string) error
	Retry(ctx context.Context, commandID, receipt string, delay time.Duration) error
	Reject(ctx context.Context, commandID, receipt, reason string) error
	PublishEvent(ctx context.Context, event hmessaging.EventEnvelope) error
}

// ExecuteRemoteMode consumes durable runner commands and creates Kubernetes Jobs.
func ExecuteRemoteMode(ctx context.Context, cfg *config.RemoteModeConfiguration, programLevel *slog.LevelVar) error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})).
		With(slog.String("runnerId", cfg.RunnerId)))

	var k8sClient *kubernetes.Clientset
	if config, err := rest.InClusterConfig(); err != nil {
		return errors.Wrap(err, "failed to create in-cluster config")
	} else {
		if k8sClient, err = kubernetes.NewForConfig(config); err != nil {
			return errors.Wrap(err, "failed to create cluster client")
		}
	}

	client, err := gatewayapi.NewClient(gatewayapi.ClientConfig{
		BaseURL: cfg.Gateway.URL, OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerId,
		PrivateKey: []byte(cfg.PrivateKey), CAFile: cfg.Gateway.CAFile,
		ClientCertFile: cfg.Gateway.ClientCertFile, ClientKeyFile: cfg.Gateway.ClientKeyFile,
	})
	if err != nil {
		return errors.Wrap(err, "configure runner gateway client")
	}
	jobs := kubernetesjob.NewK8sJobsClient(k8sClient)
	var consecutiveFailures uint64
	for {
		delivery, err := client.NextCommand(ctx)
		if err != nil {
			if errors.Is(err, gatewayapi.ErrNoCommand) {
				consecutiveFailures = 0
				continue
			}
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			consecutiveFailures++
			delay := retryDelay(consecutiveFailures)
			slog.WarnContext(ctx, "runner gateway unavailable; retrying", "err", err, "delay", delay)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		consecutiveFailures = 0
		if err := executeCommand(ctx, cfg, client, jobs, delivery); err != nil {
			slog.ErrorContext(ctx, "failed to execute runner command", "err", err, "attempt", delivery.Attempt)
		}
	}
}

func executeCommand(ctx context.Context, cfg *config.RemoteModeConfiguration, client remoteGateway, jobs kubernetesjob.K8sJobsClientInterface, delivery gatewayapi.CommandResponse) error {
	var envelope hmessaging.CommandEnvelope
	if err := json.Unmarshal(delivery.Command, &envelope); err != nil {
		return rejectCommand(ctx, client, "unknown", delivery.Receipt, errors.Wrap(err, "decode command envelope"))
	}
	if envelope.ProtocolVersion != hmessaging.ProtocolVersionV1 || envelope.OrganizationID != cfg.OrgID || envelope.RunnerID != cfg.RunnerId {
		return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, errors.New("command envelope identity or protocol version does not match this runner"))
	}
	if !envelope.ExpiresAt.IsZero() && time.Now().After(envelope.ExpiresAt) {
		_ = publishRunnerError(ctx, client, envelope, "COMMAND_EXPIRED", "command expired before execution", false)
		return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, errors.New("command expired before execution"))
	}
	if envelope.Type != hmessaging.CommandTypeCreateJob {
		return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, errors.Errorf("unsupported runner command type %q", envelope.Type))
	}
	var command hmessaging.CreateJobCommand
	if err := json.Unmarshal(envelope.Payload, &command); err != nil {
		return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, errors.Wrap(err, "decode command payload"))
	}
	err := handleCreateJob(ctx, command, jobs, cfg)
	if errors.Is(err, ErrConflict) {
		err = nil
	}
	if err != nil {
		if isPermanentJobCreationError(err) {
			_ = publishRunnerError(ctx, client, envelope, "REMOTE_RUNNER", err.Error(), false)
			return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, err)
		}
		if delivery.Attempt >= defaultCommandDeliveries {
			_ = publishRunnerError(ctx, client, envelope, "REMOTE_RUNNER", err.Error(), false)
			return rejectCommand(ctx, client, envelope.CommandID, delivery.Receipt, err)
		}
		_ = publishRunnerError(ctx, client, envelope, "REMOTE_RUNNER", err.Error(), true)
		_ = client.Retry(ctx, envelope.CommandID, delivery.Receipt, retryDelay(delivery.Attempt))
		return err
	}
	payload, _ := json.Marshal(map[string]string{"job_id": command.JobID, "namespace": command.Namespace})
	if err := publishEvent(ctx, client, envelope, "job-created", payload); err != nil {
		_ = client.Retry(ctx, envelope.CommandID, delivery.Receipt, retryDelay(delivery.Attempt))
		return err
	}
	if err := client.Acknowledge(ctx, envelope.CommandID, delivery.Receipt); err != nil {
		return err
	}
	go monitorJobScheduling(ctx, cfg, client, jobs, envelope, command)
	return nil
}

const (
	jobSchedulingMonitorInterval = 5 * time.Second
	jobSchedulingMonitorTimeout  = time.Hour
)

func monitorJobScheduling(
	ctx context.Context,
	cfg *config.RemoteModeConfiguration,
	publisher remoteGateway,
	jobs kubernetesjob.K8sJobsClientInterface,
	envelope hmessaging.CommandEnvelope,
	command hmessaging.CreateJobCommand,
) {
	delay := cfg.PodSchedulingDelay
	if delay <= 0 {
		delay = 5 * time.Minute
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	ticker := time.NewTicker(jobSchedulingMonitorInterval)
	defer ticker.Stop()
	timeout := time.NewTimer(jobSchedulingMonitorTimeout)
	defer timeout.Stop()
	for {
		message, done, err := jobSchedulingState(ctx, jobs, command.Namespace, command.JobID)
		if err != nil {
			slog.WarnContext(ctx, "failed to inspect runner job scheduling", "job_id", command.JobID, "err", err)
		} else if done {
			if message != "" {
				if err := publishRunnerError(ctx, publisher, envelope, "REMOTE_RUNNER", message, false); err != nil {
					slog.WarnContext(ctx, "failed to publish runner job scheduling error", "job_id", command.JobID, "err", err)
				} else {
					return
				}
			} else {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			slog.WarnContext(ctx, "stopped monitoring runner job scheduling after timeout", "job_id", command.JobID)
			return
		case <-ticker.C:
		}
	}
}

func jobSchedulingState(ctx context.Context, jobs kubernetesjob.K8sJobsClientInterface, namespace, jobID string) (message string, done bool, err error) {
	status, err := jobs.CheckJobStatus(ctx, namespace, jobID)
	if err != nil {
		return "", false, err
	}
	if status.Succeeded > 0 || status.Failed > 0 || (status.Ready != nil && *status.Ready > 0) {
		return "", true, nil
	}

	objectName := jobID
	pod, podErr := jobs.GetPodJob(ctx, namespace, jobID)
	if podErr == nil {
		if pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodUnknown {
			return "", true, nil
		}
		objectName = pod.Name
	} else if !errors.Is(podErr, kubernetesjob.ErrNotFound) {
		return "", false, podErr
	}

	warnings, err := jobs.GetObjectWarningEvents(ctx, namespace, objectName)
	if err != nil {
		return "", false, err
	}
	if len(warnings) == 0 {
		return "", false, nil
	}
	return strings.Join(warnings, "\n"), true, nil
}

func isPermanentJobCreationError(err error) bool {
	return k8serrors.IsBadRequest(err) ||
		k8serrors.IsForbidden(err) ||
		k8serrors.IsInvalid(err) ||
		k8serrors.IsNotFound(err) ||
		k8serrors.IsUnauthorized(err)
}

func rejectCommand(ctx context.Context, client remoteGateway, commandID, receipt string, cause error) error {
	if err := client.Reject(context.WithoutCancel(ctx), commandID, receipt, cause.Error()); err != nil {
		return errors.Wrap(err, "reject runner command")
	}
	return cause
}

const defaultCommandDeliveries = 10

func retryDelay(attempt uint64) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * time.Second
}

func publishEvent(ctx context.Context, publisher remoteGateway, command hmessaging.CommandEnvelope, eventType string, payload []byte) error {
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1,
		EventID:         command.CommandID + ":" + eventType, CommandID: command.CommandID,
		OrganizationID: command.OrganizationID, RunnerID: command.RunnerID,
		DeploymentID: command.DeploymentID, Type: eventType, CreatedAt: time.Now().UTC(), Payload: payload,
	}
	return publisher.PublishEvent(ctx, event)
}

func publishRunnerError(ctx context.Context, publisher remoteGateway, command hmessaging.CommandEnvelope, code, message string, retryable bool) error {
	payload, _ := json.Marshal(map[string]interface{}{"job_id": command.DeploymentID, "code": code, "message": message, "retryable": retryable})
	eventType := "runner-error-terminal"
	if retryable {
		eventType = "runner-error-retryable"
	}
	return publishEventAs(ctx, publisher, command, "runner-error", eventType, payload)
}

func publishEventAs(ctx context.Context, publisher remoteGateway, command hmessaging.CommandEnvelope, eventType, eventIDSuffix string, payload []byte) error {
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1,
		EventID:         command.CommandID + ":" + eventIDSuffix, CommandID: command.CommandID,
		OrganizationID: command.OrganizationID, RunnerID: command.RunnerID,
		DeploymentID: command.DeploymentID, Type: eventType, CreatedAt: time.Now().UTC(), Payload: payload,
	}
	return publisher.PublishEvent(ctx, event)
}

func handleCreateJob(ctx context.Context, command hmessaging.CreateJobCommand, k8sClient kubernetesjob.K8sJobsClientInterface, cfg *config.RemoteModeConfiguration) error {
	if command.JobID == "" || command.Namespace == "" || command.Configuration == nil {
		return errors.New("create-job command requires job_id, namespace, and configuration")
	}
	slog.InfoContext(ctx, "received command to create a job", "job_id", command.JobID, "namespace", command.Namespace)
	jobSpecJSON, err := json.Marshal(command.Configuration)
	if err != nil {
		return errors.Wrap(err, "failed to marshal job spec")
	}
	var jobSpec v1.JobSpec
	if err := json.Unmarshal(jobSpecJSON, &jobSpec); err != nil {
		return errors.Wrap(err, "failed to unmarshal job spec")
	}
	injectJobGatewayConfiguration(&jobSpec, cfg)
	if _, err := k8sClient.CreateJob(ctx, command.Namespace, &v1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: command.JobID, Namespace: command.Namespace},
		Spec:       jobSpec,
	}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			slog.InfoContext(ctx, "job already exists, skipping creation", "job_id", command.JobID, "namespace", command.Namespace)
			return ErrConflict
		}
		return errors.Wrap(err, "failed to create job")
	}
	slog.InfoContext(ctx, "job created successfully", "job_id", command.JobID, "namespace", command.Namespace)
	return nil
}

func injectJobGatewayConfiguration(jobSpec *v1.JobSpec, cfg *config.RemoteModeConfiguration) {
	url := cfg.Gateway.URL
	if url == "" {
		return
	}
	common := []corev1.EnvVar{
		{Name: "RUNNER_GATEWAY_URL", Value: url},
		{Name: "ORG_ID", Value: cfg.OrgID},
		{Name: "RUNNER_ID", Value: cfg.RunnerId},
	}
	outboxPVC := os.Getenv("RUNNER_GATEWAY_JOB_OUTBOX_PVC")
	if outboxPVC != "" {
		common = append(common, corev1.EnvVar{Name: "RUNNER_GATEWAY_OUTBOX_DIR", Value: "/var/lib/platform-orchestrator-runner/outbox"})
		jobSpec.Template.Spec.Volumes = append(jobSpec.Template.Spec.Volumes, corev1.Volume{
			Name:         "runner-gateway-outbox",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: outboxPVC}},
		})
	}
	caSecret := os.Getenv("RUNNER_GATEWAY_JOB_CA_SECRET")
	if caSecret != "" {
		caKey := os.Getenv("RUNNER_GATEWAY_JOB_CA_KEY")
		if caKey == "" {
			caKey = "ca.crt"
		}
		common = append(common, corev1.EnvVar{Name: "RUNNER_GATEWAY_CA_FILE", Value: "/etc/runner-gateway/ca.crt"})
		jobSpec.Template.Spec.Volumes = append(jobSpec.Template.Spec.Volumes, corev1.Volume{
			Name: "runner-gateway-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: caSecret, Items: []corev1.KeyToPath{{Key: caKey, Path: "ca.crt"}},
			}},
		})
	}
	for i := range jobSpec.Template.Spec.Containers {
		container := &jobSpec.Template.Spec.Containers[i]
		if outboxPVC != "" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "runner-gateway-outbox", MountPath: "/var/lib/platform-orchestrator-runner/outbox"})
		}
		if caSecret != "" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "runner-gateway-ca", MountPath: "/etc/runner-gateway", ReadOnly: true})
		}
		for _, variable := range common {
			found := -1
			for index, existing := range container.Env {
				if existing.Name == variable.Name {
					found = index
					break
				}
			}
			if found < 0 {
				container.Env = append(container.Env, variable)
			} else {
				container.Env[found] = variable
			}
		}
	}
}
