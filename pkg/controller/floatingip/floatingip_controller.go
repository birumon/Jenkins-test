package floatingip

import (
	"context"
	"fmt"
	"net"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	maxRetries     int = 5
	controllerName     = "FloatingIPController"
)

type FloatingIPController struct {
	kubeClientset   kubernetes.Interface
	customClientset clientset.Interface

	floatingIPLister   listers.FloatingIPLister
	floatingIPsSynced  cache.InformerSynced
	publicIPPoolLister listers.PublicIPPoolLister
	publicIPPoolSynced cache.InformerSynced

	allocator *allocator.PublicIPAllocator

	queue    workqueue.RateLimitingInterface
	recorder record.EventRecorder
}

// NewFloatingIPController returns a new floating ip controller
func NewFloatingIPController(
	kubeClientset kubernetes.Interface,
	customClientset clientset.Interface,
	floatingIPInformer informers.FloatingIPInformer,
	publicIPPoolInformer informers.PublicIPPoolInformer,
	publicIPAllocator *allocator.PublicIPAllocator) *FloatingIPController {

	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClientset.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "FloatingIPController"})

	f := &FloatingIPController{
		kubeClientset:   kubeClientset,
		customClientset: customClientset,
		allocator:       publicIPAllocator,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "floatingip"),
		recorder:        recorder,
	}

	floatingIPInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: f.onFloatingIPUpdate,
		UpdateFunc: func(old, cur interface{}) {
			f.onFloatingIPUpdate(cur)
		},
		DeleteFunc: f.onFloatingIPDelete,
	})
	f.floatingIPLister = floatingIPInformer.Lister()
	f.floatingIPsSynced = floatingIPInformer.Informer().HasSynced

	// trigger re-sync logic when publicippool is updated
	publicIPPoolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: f.onPublicIPPoolUpdate,
		UpdateFunc: func(old, cur interface{}) {
			f.onPublicIPPoolUpdate(cur)
		},
		DeleteFunc: f.onPublicIPPoolDelete,
	})
	f.publicIPPoolLister = publicIPPoolInformer.Lister()
	f.publicIPPoolSynced = publicIPPoolInformer.Informer().HasSynced

	return f
}

// onFloatingIPUpdate takes a floatingIP object and converts it into a namespaced name
// string which is then put onto the workqueue.
func (f *FloatingIPController) onFloatingIPUpdate(obj interface{}) {
	var key string
	var err error

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	f.queue.Add(key)
}

// onFloatingIPDelete is similar to onFloatingIPUpdate
func (f *FloatingIPController) onFloatingIPDelete(obj interface{}) {
	var key string
	var err error

	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	f.queue.Add(key)
}

func (f *FloatingIPController) onPublicIPPoolUpdate(obj interface{}) {

	ippool := obj.(*v1alpha1.PublicIPPool)

	if ippool.GetName() != "default-public-ippool" {
		return
	}

	if ippool.GetGeneration() != ippool.Status.ObservedGeneration {
		return
	}

	klog.Infof("default-public-ippool updated, sync floatingIPs")

	fips, err := f.floatingIPLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error listing floatingIPs: %v", err))
		return
	}

	for _, fip := range fips {
		f.queue.Add(fip.GetNamespace() + "/" + fip.GetName())
	}
}

func (f *FloatingIPController) onPublicIPPoolDelete(obj interface{}) {
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

	klog.Infof("default-public-ippool deleted, sync floatingIPs")

	fips, err := f.floatingIPLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Error listing floatingIPs: %v", err))
		return
	}

	for _, fip := range fips {
		f.queue.Add(fip.GetNamespace() + "/" + fip.GetName())
	}
}

func (f *FloatingIPController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer f.queue.ShutDown()

	klog.Info("Starting floatingip controller")
	defer klog.Info("Shutting down floatingip controller")

	if !cache.WaitForCacheSync(stopCh, f.floatingIPsSynced, f.publicIPPoolSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(f.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (f *FloatingIPController) worker() {
	for f.processNextWorkItem() {
	}
}

func (f *FloatingIPController) processNextWorkItem() bool {
	sKey, quit := f.queue.Get()
	if quit {
		return false
	}
	defer f.queue.Done(sKey)

	err := f.syncFloatingIP(sKey.(string))
	f.handleErr(err, sKey)

	return true
}

func (f *FloatingIPController) handleErr(err error, key interface{}) {
	if err == nil {
		f.queue.Forget(key)
		return
	}

	if f.queue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing FloatingIP %v: %v", key, err)

		f.queue.AddRateLimited(key)
		return
	}

	klog.Infof("Dropping fip %q out of the queue: %v", key, err)
	f.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (f *FloatingIPController) syncFloatingIP(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	floatingip, err := f.floatingIPLister.FloatingIPs(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Case: Floating IP deleted
			klog.Infof("FloatingIP %s deleted", key)
			// Just release public IP.
			f.allocator.FreeFloatingIP(key)

			return nil
		}

		return err
	}

	// Case: FloatingIP created, updated
	klog.Infof("FloatingIP %s created/updated", key)

	fipCopy := floatingip.DeepCopy()
	f.syncFloatingIPStatus(key, fipCopy)
	if floatingip.Status.AllocatedIP == fipCopy.Status.AllocatedIP {
		klog.Infof("No status change for floatingIP %s ", key)
		return nil
	}

	err = f.updateFloatingIPStatus(fipCopy)
	if err != nil {
		klog.Errorf("%s", err)
		return err
	}

	return nil
}

// caller must pass deepcopied object!
func (f *FloatingIPController) syncFloatingIPStatus(key string, fip *v1alpha1.FloatingIP) bool {

	// publicIP == Current public IP
	var statusIP net.IP
	if fip.Status.AllocatedIP != "" {
		statusIP = net.ParseIP(fip.Status.AllocatedIP)
	}
	// In case of status IP is malformed
	if statusIP == nil {
		// Clear status field first (it will be normalized)
		f.clearFloatingIPStatus(key, fip)
	}

	var allocatedIP string
	var err error

	// Pool changed
	// AllocateFloatingIP MUST return same IP if floating IP is allocated before,
	// and return error if status IP is not allowed.
	if statusIP != nil {
		allocatedIP, err = f.allocator.AllocateFloatingIP(key, statusIP.String())
		if err != nil {
			klog.Infof("Status IP %s not allowed, clear status: %s", statusIP.String(), err.Error())
			f.clearFloatingIPStatus(key, fip)
			statusIP = nil
		}
	}

	// User changed desired external IP
	// Clear status and try to allocate new external IP.
	if fip.Spec.ExternalIP != "" && fip.Spec.ExternalIP != statusIP.String() {
		klog.Infof("User changed desired IP from %s to %s", statusIP.String(), fip.Spec.ExternalIP)
		f.clearFloatingIPStatus(key, fip)
		statusIP = nil
	}

	if statusIP == nil {
		klog.Infof("Allocating public IP for floatingIP %s (desired: %s)", key, fip.Spec.ExternalIP)
		allocatedIP, err = f.allocator.AllocateFloatingIP(key, fip.Spec.ExternalIP)
		if err != nil {
			klog.Errorf("Public IP allocation failed for floatingIP %s: %s", key, err.Error())
			f.clearFloatingIPStatus(key, fip)
			statusIP = nil
		}
	}

	if allocatedIP == "" {
		klog.Errorf("didn't allocate an IP, but also did not fail for floatingIP %s", key)
		f.clearFloatingIPStatus(key, fip)
	}

	fip.Status.AllocatedIP = allocatedIP

	return true
}

// updateFloatingIPStatus updates floating IP status. fip must be deepcopied object.
func (f *FloatingIPController) updateFloatingIPStatus(fip *v1alpha1.FloatingIP) error {
	_, err := f.customClientset.NetworkV1alpha1().FloatingIPs(fip.GetNamespace()).UpdateStatus(context.TODO(), fip, metav1.UpdateOptions{})
	if err != nil {
		klog.Error("Error updating floatingIP status: %s", err.Error())
	}

	return err
}

// clearFloatingIPStatus release floatingIP status ip and clear status subresource.
func (f *FloatingIPController) clearFloatingIPStatus(key string, fip *v1alpha1.FloatingIP) {
	// Release IP
	f.allocator.FreeFloatingIP(key)

	// Clear status field
	fip.Status.AllocatedIP = ""
}
