package kubernetesjob

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "test-namespace"
	testJobName   = "test-job"
)

func TestNewK8sJobsClient(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewK8sJobsClient(fakeClientset)

	assert.NotNil(t, client)
	assert.Implements(t, (*K8sJobsClientInterface)(nil), client)
}

func TestK8sJobsClient_CreateJob_Success(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewK8sJobsClient(fakeClientset)

	testJob := &v1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testNamespace,
		},
		Spec: v1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "test-container",
						Image: "test-image",
					}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	createdJob, err := client.CreateJob(context.Background(), testNamespace, testJob)
	require.NoError(t, err)
	assert.NotNil(t, createdJob)
	assert.Equal(t, testJobName, createdJob.Name)
	assert.Equal(t, testNamespace, createdJob.Namespace)

	retrievedJob, err := fakeClientset.BatchV1().Jobs(testNamespace).Get(context.Background(), testJobName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, testJobName, retrievedJob.Name)
}

func TestK8sJobsClient_CreateJob_K8sCreateError(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()

	fakeClientset.PrependReactor("create", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &v1.Job{}, fmt.Errorf("simulated k8s create error")
	})

	client := NewK8sJobsClient(fakeClientset)

	testJob := &v1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testNamespace,
		},
	}

	_, err := client.CreateJob(context.Background(), testNamespace, testJob)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated k8s create error")
}

func TestK8sJobsClient_CheckJobStatus_JobExists(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()

	job := &v1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testNamespace,
		},
		Status: v1.JobStatus{
			Active:    1,
			Succeeded: 0,
			Failed:    0,
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}
	_, err := fakeClientset.BatchV1().Jobs(testNamespace).Create(context.Background(), job, metav1.CreateOptions{})
	require.NoError(t, err)

	client := NewK8sJobsClient(fakeClientset)

	status, err := client.CheckJobStatus(context.Background(), testNamespace, testJobName)
	require.NoError(t, err)
	assert.NotNil(t, status)
	assert.Equal(t, int32(1), status.Active)
	assert.Equal(t, int32(0), status.Succeeded)
	assert.Equal(t, int32(0), status.Failed)
}

func TestK8sJobsClient_CheckJobStatus_JobNotFound(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewK8sJobsClient(fakeClientset)

	status, err := client.CheckJobStatus(context.Background(), testNamespace, "non-existent-job")
	require.Error(t, err)
	assert.Nil(t, status)
	assert.Equal(t, ErrNotFound, err)
}

func TestK8sJobsClient_CheckJobStatus_K8sError(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()

	fakeClientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("simulated k8s get error")
	})

	client := NewK8sJobsClient(fakeClientset)

	status, err := client.CheckJobStatus(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, status)
	assert.Contains(t, err.Error(), "failed to retrieve job")
}

func TestKubernetesClient_GetPodJob_NoPods(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	client := NewK8sJobsClient(fakeClient)

	pod, err := client.GetPodJob(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Equal(t, ErrNotFound, err)
}

func TestKubernetesClient_GetPodJob_ListError(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("simulated k8s list error")
	})

	client := NewK8sJobsClient(fakeClient)

	pod, err := client.GetPodJob(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Contains(t, err.Error(), "failed to list pods for job")
}

func TestKubernetesClient_GetPodJob_ListNotFound(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	})

	client := NewK8sJobsClient(fakeClient)

	pod, err := client.GetPodJob(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Equal(t, ErrNotFound, err)
}

func TestKubernetesClient_GetPodJob_ListForbidden(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("access denied"))
	})

	client := NewK8sJobsClient(fakeClient)

	pod, err := client.GetPodJob(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Equal(t, ErrK8sActionForbidden, err)
}

func TestKubernetesClient_GetJobWarningEvents_WithJobEvents(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	event1 := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warning-event-1",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
		},
		InvolvedObject: corev1.ObjectReference{
			Name: testJobName,
		},
		Type:    "Warning",
		Reason:  "FailedScheduling",
		Message: "insufficient resources",
	}

	event2 := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warning-event-2",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Minute)},
		},
		InvolvedObject: corev1.ObjectReference{
			Name: testJobName,
		},
		Type:    "Warning",
		Reason:  "FailedMount",
		Message: "volume not found",
	}

	// Create a normal event (should be filtered out)
	normalEvent := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "normal-event",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		InvolvedObject: corev1.ObjectReference{
			Name: testJobName,
		},
		Type:    "Normal",
		Reason:  "Started",
		Message: "job started successfully",
	}

	_, err := fakeClient.CoreV1().Events(testNamespace).Create(context.Background(), event1, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = fakeClient.CoreV1().Events(testNamespace).Create(context.Background(), event2, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = fakeClient.CoreV1().Events(testNamespace).Create(context.Background(), normalEvent, metav1.CreateOptions{})
	require.NoError(t, err)

	client := NewK8sJobsClient(fakeClient)

	warnings, err := client.GetObjectWarningEvents(context.Background(), testNamespace, testJobName)
	require.NoError(t, err)
	assert.Len(t, warnings, 2)

	assert.Contains(t, warnings[0], "FailedMount")
	assert.Contains(t, warnings[0], "volume not found")
	assert.Contains(t, warnings[1], "FailedScheduling")
	assert.Contains(t, warnings[1], "insufficient resources")
}

func TestKubernetesClient_GetJobWarningEvents_WithPodEvents(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: testNamespace,
			Labels: map[string]string{
				"job-name": testJobName,
			},
		},
	}
	_, err := fakeClient.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	podEvent := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pod-warning-event",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		InvolvedObject: corev1.ObjectReference{
			Name: "test-pod",
		},
		Type:    "Warning",
		Reason:  "FailedPullImage",
		Message: "image not found",
	}

	_, err = fakeClient.CoreV1().Events(testNamespace).Create(context.Background(), podEvent, metav1.CreateOptions{})
	require.NoError(t, err)

	client := NewK8sJobsClient(fakeClient)

	warnings, err := client.GetObjectWarningEvents(context.Background(), testNamespace, testJobName)
	require.NoError(t, err)
	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "FailedPullImage")
	assert.Contains(t, warnings[0], "image not found")
}

func TestKubernetesClient_GetObjectWarningEvents_NoEvents(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	client := NewK8sJobsClient(fakeClient)

	warnings, err := client.GetObjectWarningEvents(context.Background(), testNamespace, testJobName)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestKubernetesClient_GetObjectWarningEvents_ListEventsError(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("simulated k8s list events error")
	})

	client := NewK8sJobsClient(fakeClient)

	warnings, err := client.GetObjectWarningEvents(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Contains(t, err.Error(), "failed to retrieve events for object")
}

func TestKubernetesClient_GetObjectWarningEvents_EventsForbiddenError(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", fmt.Errorf("access denied"))
	})

	client := NewK8sJobsClient(fakeClient)

	warnings, err := client.GetObjectWarningEvents(context.Background(), testNamespace, testJobName)
	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Equal(t, ErrK8sActionForbidden, err)
}
