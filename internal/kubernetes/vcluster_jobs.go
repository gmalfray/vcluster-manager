package kubernetes

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
)

// VClusterJobState is what can be read of a Job running inside a vcluster,
// right now, without waiting.
//
// WaitForJobComplete does the same read but blocks for up to ten minutes. That
// suits a goroutine and not a reconcile loop: an operator killed during the
// wait loses the wait. Here we look and hand back; requeueing is the caller's
// business, and the deadline can then live in the object's status instead of in
// a process that may not survive.
type VClusterJobState struct {
	// Found is false when the Job does not exist (yet, or any more — it carries
	// ttlSecondsAfterFinished and disappears on its own).
	Found bool
	// Complete and Failed mirror the Job's own conditions.
	Complete bool
	Failed   bool
	// Detail carries the failure message when there is one.
	Detail string
	// StartedAt is status.startTime — the natural anchor for bounding a wait
	// without having to write down when we started.
	StartedAt time.Time
}

// GetVClusterJobState reads one Job inside a vcluster.
//
// An error means "could not look", not "not there": during the grace period the
// vcluster is scaled to zero, so its API answers nothing and the two cases must
// not be confused by the caller.
func (s *StatusClient) GetVClusterJobState(ctx context.Context, name, jobName, jobNamespace string) (VClusterJobState, error) {
	var st VClusterJobState
	err := s.withVClusterClientset(ctx, name, func(cs *k8sclient.Clientset) error {
		job, err := cs.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading job %s/%s: %w", jobNamespace, jobName, err)
		}
		st.Found = true
		if job.Status.StartTime != nil {
			st.StartedAt = job.Status.StartTime.Time
		}
		for _, c := range job.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				st.Complete = true
			case batchv1.JobFailed:
				st.Failed = true
				st.Detail = c.Message
			}
		}
		return nil
	})
	return st, err
}
