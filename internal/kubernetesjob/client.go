//go:generate go tool mockgen -destination=mocks/client.go -package mock_k8s github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob K8sJobsClientInterface

package kubernetesjob

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/pkg/errors"
	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var ErrNotFound = errors.New("job not found")
var ErrK8sActionForbidden = errors.New("kubernetes action forbidden")

type K8sJobsClientInterface interface {
	CreateJob(ctx context.Context, namespace string, job *v1.Job) (*v1.Job, error)
	CheckJobStatus(ctx context.Context, namespace, deploymentId string) (*v1.JobStatus, error)
	GetPodJob(ctx context.Context, namespace, jobName string) (*corev1.Pod, error)
	GetObjectWarningEvents(ctx context.Context, namespace string, jobName string) ([]string, error)
}

type k8sJobsClient struct {
	clientset kubernetes.Interface
}

func NewK8sJobsClient(clientset kubernetes.Interface) K8sJobsClientInterface {
	return &k8sJobsClient{clientset: clientset}
}

func (k *k8sJobsClient) CreateJob(ctx context.Context, namespace string, job *v1.Job) (*v1.Job, error) {
	return k.clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
}

func (k *k8sJobsClient) CheckJobStatus(ctx context.Context, namespace string, jobName string) (*v1.JobStatus, error) {
	if job, err := k.clientset.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{}); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, errors.Wrap(err, "failed to retrieve job")
	} else {
		return &job.Status, nil
	}
}

func (k *k8sJobsClient) GetObjectWarningEvents(ctx context.Context, namespace string, objName string) ([]string, error) {
	eventList, err := k.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", objName),
	})
	if err != nil {
		if k8serrors.IsForbidden(err) {
			return nil, ErrK8sActionForbidden
		}
		return nil, errors.Wrapf(err, "failed to retrieve events for object %q", objName)
	}

	slices.SortFunc(eventList.Items, func(event1, event2 corev1.Event) int {
		return cmp.Compare(event2.CreationTimestamp.Unix(), event1.CreationTimestamp.Unix())
	})
	var warnings []string
	for _, event := range eventList.Items {
		if event.Type == corev1.EventTypeWarning {
			warnings = append(warnings, fmt.Sprintf("%s - %s - %s", event.CreationTimestamp.UTC(), event.Reason, event.Message))
		}
	}
	return warnings, nil
}

func (k *k8sJobsClient) GetPodJob(ctx context.Context, namespace, jobName string) (*corev1.Pod, error) {
	podList, err := k.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, ErrNotFound
		} else if k8serrors.IsForbidden(err) {
			return nil, ErrK8sActionForbidden
		}
		return nil, errors.Wrapf(err, "failed to list pods for job '%s'", jobName)
	}
	if podList == nil || len(podList.Items) == 0 {
		return nil, ErrNotFound
	}
	// We expect only one pod per job
	return &podList.Items[0], nil
}
