package service

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	"stonelb/pkg/allocator"
	"stonelb/pkg/apis/network/v1alpha1"
	clientset "stonelb/pkg/generated/clientset/versioned"
	informers "stonelb/pkg/generated/informers/externalversions/network/v1alpha1"
	listers "stonelb/pkg/generated/listers/network/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	maxRetries           int = 5
	controllerName           = "ServiceController"
	SuccessSynced            = "Synced"
	ErrResourceExists        = "ErrResourceExists"
	MessageServiceSynced     = "Service synced successfully"
)

// ServiceController is the controller implementation for Service resources
type ServiceController struct {
	kubeClientset   kubernetes.Interface
	customClientset clientset.Interface

	serviceLister      corelisters.ServiceLister
	servicesSynced     cache.InformerSynced
	publicIPPoolLister listers.PublicIPPoolLister
	publicIPPoolSynced cache.InformerSynced

	allocator *allocator.PublicIPAllocator

	queue    workqueue.RateLimitingInterface
	recorder record.EventRecorder
}

// NewServiceController returns a new service controller
func NewServiceController(
	kubeClientset kubernetes.Interface,
	customClientset clientset.Interface,
	serviceInformer coreinformers.ServiceInformer,
	publicIPPoolInformer informers.PublicIPPoolInformer,
	publicIPAllocator *allocator.PublicIPAllocator) *ServiceController {

	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClientset.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})

	s := &ServiceController{
		kubeClientset:   kubeClientset,
		customClientset: customClientset,
		allocator:       publicIPAllocator,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "service"),
		recorder:        recorder,
	}

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: s.onServiceUpdate,
		UpdateFunc: func(old, cur interface{}) {
			s.onServiceUpdate(cur)
		},
		DeleteFunc: s.onServiceDelete,
	})
	s.serviceLister = serviceInformer.Lister()
	s.servicesSynced = serviceInformer.Informer().HasSynced

	// re-sync logic triggered when publicippool is updated
	publicIPPoolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: s.onPublicIPPoolUpdate,
		UpdateFunc: func(old, cur interface{}) {
			s.onPublicIPPoolUpdate(cur)
		},
		DeleteFunc: s.onPublicIPPoolDelete,
	})
	s.publicIPPoolLister = publicIPPoolInformer.Lister()
	s.publicIPPoolSynced = publicIPPoolInformer.Informer().HasSynced

	return s
}

// onServiceUpdate takes a service object and converts it into a namespaced name
// string which is then put onto the workqueue.
func (s *ServiceController) onServiceUpdate(obj interface{}) {
	var key string
	var err error

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	s.queue.Add(key)
}

// onServiceDelete is similar to onServiceUpdate... modify if we need deletion custom logic
func (s *ServiceController) onServiceDelete(obj interface{}) {
	var key string
	var err error

	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	s.queue.Add(key)
}

func (s *ServiceController) onPublicIPPoolUpdate(obj interface{}) {

	ippool := obj.(*v1alpha1.PublicIPPool)

	if ippool.GetName() != "default-public-ippool" {
		return
	}

	if ippool.GetGeneration() != ippool.Status.ObservedGeneration {
		return
	}

	klog.Infof("default-public-ippool updated, sync loadbalancer services")

	svcs, err := s.serviceLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error listing services: %v", err))
		return
	}

	for _, svc := range svcs {
		if svc.Spec.Type == "LoadBalancer" {
			s.queue.Add(svc.GetNamespace() + "/" + svc.GetName())
		}
	}
}

func (s *ServiceController) onPublicIPPoolDelete(obj interface{}) {
	var key string
	var err error

	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't split name for key %+v: %v", key, err))
		return
	}

	if name != "default-public-ippool" {
		return
	}

	klog.Infof("default-public-ippool deleted, sync loadbalancer services")

	svcs, err := s.serviceLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error listing services: %v", err))
		return
	}

	for _, svc := range svcs {
		if svc.Spec.Type == "LoadBalancer" {
			s.queue.Add(svc.GetNamespace() + "/" + svc.GetName())
		}
	}
}

func (s *ServiceController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer s.queue.ShutDown()

	klog.Info("Starting service controller")
	defer klog.Info("Shutting down service controller")

	if !cache.WaitForCacheSync(stopCh, s.servicesSynced, s.publicIPPoolSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(s.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (s *ServiceController) worker() {
	for s.processNextWorkItem() {
	}
}

func (s *ServiceController) processNextWorkItem() bool {
	sKey, quit := s.queue.Get()
	if quit {
		return false
	}
	defer s.queue.Done(sKey)

	err := s.syncService(sKey.(string))
	s.handleErr(err, sKey)

	return true
}

func (s *ServiceController) handleErr(err error, key interface{}) {
	if err == nil {
		s.queue.Forget(key)
		return
	}

	if s.queue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing Service %v: %v", key, err)

		s.queue.AddRateLimited(key)
		return
	}

	klog.Infof("Dropping svc %q out of the queue: %v", key, err)
	s.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (s *ServiceController) syncService(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	service, err := s.serviceLister.Services(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Case: Service deleted
			klog.Infof("Service %s deleted", key)
			// Just release public IP.
			s.allocator.FreeService(key)

			return nil
		}

		return err
	}

	// Case: Service created, updated
	klog.Infof("Service %s created/updated", key)

	svcCopy := service.DeepCopy()
	s.syncServiceStatus(key, svcCopy)
	if reflect.DeepEqual(service.Status, svcCopy.Status) {
		klog.Infof("No status change for service %s ", key)
		return nil
	}

	err = s.updateServiceStatus(svcCopy)
	if err != nil {
		klog.Errorf("%s", err)
		return err
	}

	return nil
}

// caller must pass deepcopied service object!
func (s *ServiceController) syncServiceStatus(key string, svc *corev1.Service) bool {
	// Check if service is LoadBalancer type.
	// If not a LB type, then clear status field.
	if svc.Spec.Type != "LoadBalancer" {
		if len(svc.Status.LoadBalancer.Ingress) >= 1 {
			klog.Infof("Service %s is not LoadBalancer type, clear status", key)
			s.clearServiceStatus(key, svc)
		}
		return true
	}

	// lbIP == Current LB IP
	var statusLBIP net.IP
	if len(svc.Status.LoadBalancer.Ingress) == 1 {
		statusLBIP = net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	// In case of LB IP is malformed
	if statusLBIP == nil {
		// Clear status field first (it will be normalized)
		s.clearServiceStatus(key, svc)
	}

	var allocatedIP string
	var err error

	// Pool changed
	// AllocateService MUST return same IP if service is allocated before,
	// and return error if status LB IP is not allowed.
	if statusLBIP != nil {
		allocatedIP, err = s.allocator.AllocateService(key, statusLBIP.String())
		if err != nil {
			klog.Infof("Status IP not allowed, clear status: %s", err.Error())
			s.clearServiceStatus(key, svc)
			statusLBIP = nil
		}
	}

	// User changed desired LB IP
	// Clear status and try to allocate new LoadBalancer IP.
	if svc.Spec.LoadBalancerIP != "" && svc.Spec.LoadBalancerIP != statusLBIP.String() {
		klog.Infof("User changed desired IP from %s to %s", statusLBIP.String(), svc.Spec.LoadBalancerIP)
		s.clearServiceStatus(key, svc)
		statusLBIP = nil
	}

	// No public IP allocated, so allocate public IP.
	if statusLBIP == nil {
		klog.Infof("Allocating public IP for service %s (desired: %s)", key, svc.Spec.LoadBalancerIP)
		allocatedIP, err = s.allocator.AllocateService(key, svc.Spec.LoadBalancerIP)
		if err != nil {
			klog.Errorf("Public IP allocation failed for service %s: %s", key, err.Error())
			s.clearServiceStatus(key, svc)
			statusLBIP = nil
		}
	}

	if allocatedIP == "" {
		klog.Infof("didn't allocate an IP, but also did not fail for service %s", key)
		s.clearServiceStatus(key, svc)
	}

	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: allocatedIP}}

	return true
}

// updateServiceStatus updates service status. svc must be deepcopied object.
func (s *ServiceController) updateServiceStatus(svc *corev1.Service) error {
	_, err := s.kubeClientset.CoreV1().Services(svc.Namespace).UpdateStatus(context.TODO(), svc, metav1.UpdateOptions{})
	if err != nil {
		klog.Error("Error update service status: %s", err.Error())
	}

	return err
}

// clearServiceStatus release service status ip and clear status subresource.
func (s *ServiceController) clearServiceStatus(key string, svc *corev1.Service) {
	// Release IP
	s.allocator.FreeService(key)

	// Clear status field
	svc.Status.LoadBalancer = corev1.LoadBalancerStatus{}
}
