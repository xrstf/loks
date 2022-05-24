package watcher

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"go.xrstf.de/loks/pkg/collector"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type Watcher struct {
	clientset      *kubernetes.Clientset
	log            logrus.FieldLogger
	collector      collector.Collector
	initialPods    []corev1.Pod
	opt            Options
	seenContainers sets.String
}

type Options struct {
	LabelSelector  labels.Selector
	Namespaces     []string
	ResourceNames  []string
	ContainerNames []string
	RunningOnly    bool
	OneShot        bool
}

func NewWatcher(
	clientset *kubernetes.Clientset,
	c collector.Collector,
	log logrus.FieldLogger,
	initialPods []corev1.Pod,
	opt Options,
) *Watcher {
	return &Watcher{
		clientset:      clientset,
		log:            log,
		collector:      c,
		initialPods:    initialPods,
		opt:            opt,
		seenContainers: sets.NewString(),
	}
}

func (w *Watcher) Watch(ctx context.Context, wi watch.Interface) {
	wg := sync.WaitGroup{}

	for i := range w.initialPods {
		if w.podMatchesCriteria(&w.initialPods[i]) {
			w.startLogCollectors(ctx, &wg, &w.initialPods[i])
		}
	}

	// wi can be nil if we do not want to actually watch, but instead
	// just process the initial pods (if --oneshot is given)
	if wi != nil {
		for event := range wi.ResultChan() {
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			pod := &corev1.Pod{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), pod)
			if err != nil {
				continue
			}

			if w.podMatchesCriteria(pod) {
				w.startLogCollectors(ctx, &wg, pod)
			}
		}
	}

	wg.Wait()
}

func (w *Watcher) startLogCollectors(ctx context.Context, wg *sync.WaitGroup, pod *corev1.Pod) {
	w.startLogCollectorsForContainers(ctx, wg, pod, pod.Spec.InitContainers, pod.Status.InitContainerStatuses)
	w.startLogCollectorsForContainers(ctx, wg, pod, pod.Spec.Containers, pod.Status.ContainerStatuses)
}

func (w *Watcher) startLogCollectorsForContainers(ctx context.Context, wg *sync.WaitGroup, pod *corev1.Pod, containers []corev1.Container, statuses []corev1.ContainerStatus) {
	podLog := w.getPodLog(pod)

	for _, container := range containers {
		containerName := container.Name
		containerLog := podLog.WithField("container", containerName)

		if !w.containerNameMatches(containerName) {
			containerLog.Debug("Container name does not match.")
			continue
		}

		var status *corev1.ContainerStatus
		for i, s := range statuses {
			if s.Name == containerName {
				status = &statuses[i]
				break
			}
		}

		// container has no status yet
		if status == nil {
			containerLog.Debug("Container has no status yet.")
			continue
		}

		// container sttaus not what we want
		if w.opt.RunningOnly {
			if status.State.Running == nil {
				containerLog.Debug("Container is not running.")
				continue
			}
		} else if status.State.Running == nil && status.State.Terminated == nil {
			containerLog.Debug("Container is still waiting.")
			continue
		}

		ident := fmt.Sprintf("%s:%s:%s:%d", pod.Namespace, pod.Name, containerName, status.RestartCount)

		// we have already started a collector for this incarnation of the container;
		// whenever a container restarts, we want to create a new collector with the
		// new restart count
		if w.seenContainers.Has(ident) {
			continue
		}

		// remember that we have seen this incarnation
		w.seenContainers.Insert(ident)

		wg.Add(1)
		go w.collectLogs(ctx, wg, containerLog, pod, containerName, int(status.RestartCount))
	}
}

func (w *Watcher) collectLogs(ctx context.Context, wg *sync.WaitGroup, log logrus.FieldLogger, pod *corev1.Pod, containerName string, restartCount int) {
	defer wg.Done()

	log.Info("Starting to collect logs…")

	request := w.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: containerName,
		Follow:    !w.opt.OneShot,
	})

	stream, err := request.Stream(ctx)
	if err != nil {
		log.WithError(err).Error("Failed to stream logs.")
		return
	}
	defer stream.Close()

	if err := w.collector.CollectLogs(ctx, log, pod, containerName, stream); err != nil {
		log.WithError(err).Error("Failed to collect logs.")
	}

	log.Info("Logs have finished.")
}

func (w *Watcher) getPodLog(pod *corev1.Pod) logrus.FieldLogger {
	return w.log.WithField("pod", pod.Name).WithField("namespace", pod.Namespace)
}

func (w *Watcher) podMatchesCriteria(pod *corev1.Pod) bool {
	podLog := w.getPodLog(pod)

	return w.resourceNameMatches(podLog, pod) && w.resourceNamespaceMatches(podLog, pod) && w.resourceLabelsMatches(podLog, pod)
}

func (w *Watcher) resourceNameMatches(log logrus.FieldLogger, pod *corev1.Pod) bool {
	if needleMatchesPatterns(pod.GetName(), w.opt.ResourceNames) {
		return true
	}

	log.Debug("Pod name does not match.")

	return false
}

func (w *Watcher) resourceNamespaceMatches(log logrus.FieldLogger, pod *corev1.Pod) bool {
	if needleMatchesPatterns(pod.GetNamespace(), w.opt.Namespaces) {
		return true
	}

	log.Debug("Pod namespace does not match.")

	return false
}

func (w *Watcher) resourceLabelsMatches(log logrus.FieldLogger, pod *corev1.Pod) bool {
	if w.opt.LabelSelector == nil || w.opt.LabelSelector.Matches(labels.Set(pod.Labels)) {
		return true
	}

	log.Debug("Pod labels do not match.")

	return false
}

func (w *Watcher) containerNameMatches(containerName string) bool {
	return needleMatchesPatterns(containerName, w.opt.ContainerNames)
}

func nameMatches(name string, pattern string) bool {
	if strings.Contains(pattern, "*") {
		matched, _ := filepath.Match(pattern, name)
		return matched
	}

	return name == pattern
}

func needleMatchesPatterns(needle string, patterns []string) bool {
	// no patterns given, so everything matches
	if len(patterns) == 0 {
		return true
	}

	for _, pattern := range patterns {
		if nameMatches(needle, pattern) {
			return true
		}
	}

	return false
}
