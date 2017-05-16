package main

import (
	"fmt"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

type (
	ContainerFilterFunc func(pod *v1.Pod, container *v1.Container) bool
	ContainerEnterFunc  func(pod *v1.Pod, container *v1.Container)
	ContainerExitFunc   func(pod *v1.Pod, container *v1.Container)
	ContainerErrorFunc  func(pod *v1.Pod, container *v1.Container, err error)
)

type Controller struct {
	sync.Mutex
	clientset     *kubernetes.Clientset
	tailers       map[string]*ContainerTailer
	namespace     string
	labelSelector labels.Selector
	filterFunc    ContainerFilterFunc
	eventFunc     LogEventFunc
	enterFunc     ContainerEnterFunc
	exitFunc      ContainerExitFunc
	errorFunc     ContainerErrorFunc
}

func NewController(
	clientset *kubernetes.Clientset,
	namespace string,
	labelSelector labels.Selector,
	filterFunc ContainerFilterFunc,
	eventFunc LogEventFunc,
	enterFunc ContainerEnterFunc,
	exitFunc ContainerExitFunc,
	errorFunc ContainerErrorFunc) *Controller {
	return &Controller{
		clientset:     clientset,
		tailers:       map[string]*ContainerTailer{},
		namespace:     namespace,
		labelSelector: labelSelector,
		filterFunc:    filterFunc,
		eventFunc:     eventFunc,
		enterFunc:     enterFunc,
		exitFunc:      exitFunc,
		errorFunc:     errorFunc,
	}
}

func (ctl *Controller) Run() {
	podListWatcher := cache.NewListWatchFromClient(
		ctl.clientset.CoreV1Client.RESTClient(), "pods", ctl.namespace, fields.Everything())

	_, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc:    ctl.onAdd,
		UpdateFunc: func(old interface{}, new interface{}) {},
		DeleteFunc: ctl.onDelete,
	}, cache.Indexers{})

	stopCh := make(chan struct{}, 1)
	go informer.Run(stopCh)
	<-stopCh
}

func (ctl *Controller) onAdd(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return
	}
	if !ctl.labelSelector.Matches(labels.Set(pod.Labels)) {
		return
	}
	any := false
	for _, container := range pod.Spec.Containers {
		if ctl.filterFunc(pod, &container) {
			any = true
			break
		}
	}
	if any {
		for _, container := range pod.Spec.Containers {
			ctl.addContainer(pod, &container)
		}
	}
}

func (ctl *Controller) onDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return
	}
	for _, container := range pod.Spec.Containers {
		ctl.deleteContainer(pod, &container)
	}
}

func (ctl *Controller) addContainer(pod *v1.Pod, container *v1.Container) {
	ctl.enterFunc(pod, container)

	ctl.Lock()
	defer ctl.Unlock()

	key := buildKey(pod, container)
	if _, ok := ctl.tailers[key]; !ok {
		tailer := NewContainerTailer(ctl.clientset, pod, container, ctl.eventFunc)
		go func() {
			if err := tailer.Run(); err != nil {
				ctl.errorFunc(pod, container, err)
			}
		}()
		ctl.tailers[key] = tailer
	}
}

func (ctl *Controller) deleteContainer(pod *v1.Pod, container *v1.Container) {
	ctl.Lock()
	defer ctl.Unlock()

	key := buildKey(pod, container)
	if tailer, ok := ctl.tailers[key]; ok {
		delete(ctl.tailers, key)
		tailer.Stop()
		ctl.exitFunc(pod, container)
	}
}

func buildKey(pod *v1.Pod, container *v1.Container) string {
	return fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
}