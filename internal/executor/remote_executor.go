package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"

	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var ErrConflict = errors.New("Message already handled")

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

	connection, err := natstransport.Connect(natstransport.Config{
		URL: cfg.NATS.URL, Token: cfg.NATS.Token, CredentialsFile: cfg.NATS.CredentialsFile,
		CAFile: cfg.NATS.CAFile, ClientCertFile: cfg.NATS.ClientCertFile,
		ClientKeyFile: cfg.NATS.ClientKeyFile, OutboxDir: cfg.NATS.OutboxDir,
		Name: "platform-orchestrator-runner/" + cfg.RunnerId,
	})
	if err != nil {
		return errors.Wrap(err, "failed to connect to NATS")
	}
	defer connection.Close()
	publisher, err := natstransport.NewPublisher(connection, cfg.NATS.OutboxDir)
	if err != nil {
		return err
	}
	consumer, err := natstransport.NewRunnerConsumer(connection, cfg.OrgID, cfg.RunnerId, cfg.NATS.BootstrapStreams)
	if err != nil {
		return err
	}
	jobs := kubernetesjob.NewK8sJobsClient(k8sClient)
	flushTicker := time.NewTicker(5 * time.Second)
	defer flushTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-flushTicker.C:
			if err := publisher.Flush(ctx); err != nil {
				slog.WarnContext(ctx, "failed to flush NATS outbound spool", "err", err)
			}
			if err := natstransport.FlushLogObjects(ctx, connection, cfg.NATS.OutboxDir); err != nil {
				slog.WarnContext(ctx, "failed to flush NATS log object spool", "err", err)
			}
		default:
		}
		delivery, err := consumer.Fetch(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			return errors.Wrap(err, "fetch runner command")
		}
		if err := executeCommand(ctx, cfg, publisher, jobs, delivery); err != nil {
			slog.ErrorContext(ctx, "failed to execute runner command", "err", err, "message_id", delivery.Message.ID, "attempt", delivery.Attempts)
		}
	}
}

func executeCommand(ctx context.Context, cfg *config.RemoteModeConfiguration, publisher *natstransport.Publisher, jobs kubernetesjob.K8sJobsClientInterface, delivery natstransport.Delivery) error {
	var envelope hmessaging.CommandEnvelope
	if err := json.Unmarshal(delivery.Message.Data, &envelope); err != nil {
		return deadLetter(ctx, publisher, delivery, errors.Wrap(err, "decode command envelope"))
	}
	if envelope.ProtocolVersion != hmessaging.ProtocolVersionV1 || envelope.OrganizationID != cfg.OrgID || envelope.RunnerID != cfg.RunnerId {
		return deadLetter(ctx, publisher, delivery, errors.New("command envelope identity or protocol version does not match this runner"))
	}
	if !envelope.ExpiresAt.IsZero() && time.Now().After(envelope.ExpiresAt) {
		_ = publishRunnerError(ctx, publisher, envelope, "COMMAND_EXPIRED", "command expired before execution", false)
		return deadLetter(ctx, publisher, delivery, errors.New("command expired before execution"))
	}
	if envelope.Type != hmessaging.CommandTypeCreateJob {
		return deadLetter(ctx, publisher, delivery, errors.Errorf("unsupported runner command type %q", envelope.Type))
	}
	var command hmessaging.CreateJobCommand
	if err := json.Unmarshal(envelope.Payload, &command); err != nil {
		return deadLetter(ctx, publisher, delivery, errors.Wrap(err, "decode command payload"))
	}
	err := handleCreateJob(ctx, command, jobs, cfg)
	if errors.Is(err, ErrConflict) {
		err = nil
	}
	if err != nil {
		if isPermanentJobCreationError(err) {
			_ = publishRunnerError(ctx, publisher, envelope, "REMOTE_RUNNER", err.Error(), false)
			return deadLetter(ctx, publisher, delivery, err)
		}
		if delivery.Attempts >= defaultCommandDeliveries {
			_ = publishRunnerError(ctx, publisher, envelope, "REMOTE_RUNNER", err.Error(), false)
			return deadLetter(ctx, publisher, delivery, err)
		}
		_ = publishRunnerError(ctx, publisher, envelope, "REMOTE_RUNNER", err.Error(), true)
		_ = delivery.Nak(retryDelay(delivery.Attempts))
		return err
	}
	payload, _ := json.Marshal(map[string]string{"job_id": command.JobID, "namespace": command.Namespace})
	if err := publishEvent(ctx, publisher, envelope, "job-created", payload); err != nil {
		_ = delivery.Nak(retryDelay(delivery.Attempts))
		return err
	}
	if err := delivery.Ack(); err != nil {
		return err
	}
	go monitorJobScheduling(ctx, cfg, publisher, jobs, envelope, command)
	return nil
}

const (
	jobSchedulingMonitorInterval = 5 * time.Second
	jobSchedulingMonitorTimeout  = time.Hour
)

func monitorJobScheduling(
	ctx context.Context,
	cfg *config.RemoteModeConfiguration,
	publisher *natstransport.Publisher,
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

func deadLetter(ctx context.Context, publisher *natstransport.Publisher, delivery natstransport.Delivery, cause error) error {
	subject, err := hmessaging.DeadLetterSubject(delivery.Message.Subject)
	if err != nil {
		_ = delivery.Nak(time.Second)
		return errors.Wrap(err, "build dead-letter subject")
	}
	header := delivery.Message.Header.Clone()
	if header == nil {
		header = hmessaging.Header{}
	}
	header.Set("Po-Dead-Letter-Reason", cause.Error())
	dlq := delivery.Message.Clone()
	dlq.ID += ":dlq"
	dlq.Subject = subject
	dlq.Header = header
	dlq.ExpiresAt = time.Time{}
	if err := publisher.Publish(context.WithoutCancel(ctx), dlq); err != nil {
		_ = delivery.Nak(time.Second)
		return errors.Wrap(err, "publish dead-letter message")
	}
	if err := delivery.Term(); err != nil {
		return errors.Wrap(err, "terminate dead-lettered command")
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

func publishEvent(ctx context.Context, publisher *natstransport.Publisher, command hmessaging.CommandEnvelope, eventType string, payload []byte) error {
	subject, err := hmessaging.RunnerEventSubject(command.OrganizationID, command.RunnerID, eventType)
	if err != nil {
		return err
	}
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1,
		EventID:         command.CommandID + ":" + eventType, CommandID: command.CommandID,
		OrganizationID: command.OrganizationID, RunnerID: command.RunnerID,
		DeploymentID: command.DeploymentID, Type: eventType, CreatedAt: time.Now().UTC(), Payload: payload,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return publisher.Publish(ctx, hmessaging.Message{ID: event.EventID, Subject: subject, Data: data, CreatedAt: event.CreatedAt})
}

func publishRunnerError(ctx context.Context, publisher *natstransport.Publisher, command hmessaging.CommandEnvelope, code, message string, retryable bool) error {
	payload, _ := json.Marshal(map[string]interface{}{"job_id": command.DeploymentID, "code": code, "message": message, "retryable": retryable})
	eventType := "runner-error-terminal"
	if retryable {
		eventType = "runner-error-retryable"
	}
	return publishEventAs(ctx, publisher, command, "runner-error", eventType, payload)
}

func publishEventAs(ctx context.Context, publisher *natstransport.Publisher, command hmessaging.CommandEnvelope, eventType, eventIDSuffix string, payload []byte) error {
	subject, err := hmessaging.RunnerEventSubject(command.OrganizationID, command.RunnerID, eventType)
	if err != nil {
		return err
	}
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1,
		EventID:         command.CommandID + ":" + eventIDSuffix, CommandID: command.CommandID,
		OrganizationID: command.OrganizationID, RunnerID: command.RunnerID,
		DeploymentID: command.DeploymentID, Type: eventType, CreatedAt: time.Now().UTC(), Payload: payload,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return publisher.Publish(ctx, hmessaging.Message{ID: event.EventID, Subject: subject, Data: data, CreatedAt: event.CreatedAt})
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
	injectJobNATSConfiguration(&jobSpec, cfg)
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

func injectJobNATSConfiguration(jobSpec *v1.JobSpec, cfg *config.RemoteModeConfiguration) {
	url := cfg.NATS.URL
	if url == "" {
		return
	}
	common := []corev1.EnvVar{
		{Name: "NATS_URL", Value: url},
		{Name: "ORG_ID", Value: cfg.OrgID},
		{Name: "RUNNER_ID", Value: cfg.RunnerId},
	}
	outboxPVC := os.Getenv("NATS_JOB_OUTBOX_PVC")
	if outboxPVC != "" {
		common = append(common, corev1.EnvVar{Name: "NATS_OUTBOX_DIR", Value: "/var/lib/platform-orchestrator-runner/outbox"})
		jobSpec.Template.Spec.Volumes = append(jobSpec.Template.Spec.Volumes, corev1.Volume{
			Name:         "nats-outbox",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: outboxPVC}},
		})
	}
	secret := os.Getenv("NATS_JOB_CREDENTIALS_SECRET")
	authType := os.Getenv("NATS_JOB_AUTH_TYPE")
	if authType == "" {
		authType = "token"
	}
	if secret != "" && authType == "token" {
		key := os.Getenv("NATS_JOB_TOKEN_KEY")
		if key == "" {
			key = "token"
		}
		common = append(common, corev1.EnvVar{Name: "NATS_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key},
		}})
	}
	if secret != "" && (authType == "credentials" || os.Getenv("NATS_JOB_CA_ENABLED") == "true") {
		items := []corev1.KeyToPath{}
		if authType == "credentials" {
			key := os.Getenv("NATS_JOB_CREDENTIALS_KEY")
			if key == "" {
				key = "creds"
			}
			items = append(items, corev1.KeyToPath{Key: key, Path: "creds"})
			common = append(common, corev1.EnvVar{Name: "NATS_CREDS_FILE", Value: "/etc/nats-auth/creds"})
		}
		if os.Getenv("NATS_JOB_CA_ENABLED") == "true" {
			key := os.Getenv("NATS_JOB_CA_KEY")
			if key == "" {
				key = "ca.crt"
			}
			items = append(items, corev1.KeyToPath{Key: key, Path: "ca.crt"})
			common = append(common, corev1.EnvVar{Name: "NATS_CA_FILE", Value: "/etc/nats-auth/ca.crt"})
		}
		jobSpec.Template.Spec.Volumes = append(jobSpec.Template.Spec.Volumes, corev1.Volume{
			Name: "nats-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secret, Items: items,
			}},
		})
	}
	for i := range jobSpec.Template.Spec.Containers {
		container := &jobSpec.Template.Spec.Containers[i]
		if outboxPVC != "" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "nats-outbox", MountPath: "/var/lib/platform-orchestrator-runner/outbox"})
		}
		if secret != "" && (authType == "credentials" || os.Getenv("NATS_JOB_CA_ENABLED") == "true") {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "nats-auth", MountPath: "/etc/nats-auth", ReadOnly: true})
		}
		for _, variable := range common {
			found := false
			for _, existing := range container.Env {
				if existing.Name == variable.Name {
					found = true
					break
				}
			}
			if !found {
				container.Env = append(container.Env, variable)
			}
		}
	}
}
