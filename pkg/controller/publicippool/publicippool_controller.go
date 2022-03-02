package publicippool

import (
	"context"
	"fmt"
	"stonelb/pkg/allocator"
	"stonelb/pkg/apis/network/v1alpha1"
	clientset "stonelb/pkg/generated/clientset/versioned"
	"stonelb/pkg/generated/clientset/versioned/scheme"
	informers "stonelb/pkg/generated/informers/externalversions/network/v1alpha1"
	listers "stonelb/pkg/generated/listers/network/v1alpha1"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	maxRetries     int = 5
	controllerName     = "PublicIPPoolController"
)

type PublicIPPoolController struct {
	kubeClientset   kubernetes.Interface
	customClientset clientset.Interface

	publicIPPoolLister listers.PublicIPPoolLister
	publicIPPoolSynced cache.InformerSynced

	allocator *allocator.PublicIPAllocator

	observedGeneration int64

	queue    workqueue.RateLimitingInterface
	recorder record.EventRecorder
}

func NewPublicIPPoolController(
	kubeClientset kubernetes.Interface,
	customClientset clientset.Interface,
	publicIPPoolInformer informers.PublicIPPoolInformer,
	publicIPAllocator *allocator.PublicIPAllocator) *PublicIPPoolController {

	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClientset.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})

	p := &PublicIPPoolController{
		kubeClientset:   kubeClientset,
		customClientset: customClientset,
		allocator:       publicIPAllocator,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "publicippool"),
		recorder:        recorder,
	}

	publicIPPoolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: p.onPublicIPPoolUpdate,
		UpdateFunc: func(old, cur interface{}) {
			p.onPublicIPPoolUpdate(cur)
		},
		DeleteFunc: p.onPublicIPPoolDelete,
	})
	p.publicIPPoolLister = publicIPPoolInformer.Lister()
	p.publicIPPoolSynced = publicIPPoolInformer.Informer().HasSynced

	return p
}

// onPublicIPPoolUpdate takes a publicippool object and converts it into a namespaced name
// string which is then put onto the workqueue.
func (p *PublicIPPoolController) onPublicIPPoolUpdate(obj interface{}) {
	var key string
	var err error

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	p.queue.Add(key)
}

// onPublicIPPoolDelete is similar to onPublicIPPoolUpdate... modify if we need deletion custom logic
func (p *PublicIPPoolController) onPublicIPPoolDelete(obj interface{}) {
	var key string
	var err error

	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	p.queue.Add(key)
}

func (p *PublicIPPoolController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer p.queue.ShutDown()

	klog.Info("Starting publicippool controller")
	defer klog.Info("Shutting down publicippool controller")

	if !cache.WaitForCacheSync(stopCh, p.publicIPPoolSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(p.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (p *PublicIPPoolController) worker() {
	for p.processNextWorkItem() {
	}
}

func (p *PublicIPPoolController) processNextWorkItem() bool {
	pKey, quit := p.queue.Get()
	if quit {
		return false
	}
	defer p.queue.Done(pKey)

	err := p.syncHandler(pKey.(string))
	p.handleErr(err, pKey)

	return true
}

func (p *PublicIPPoolController) handleErr(err error, key interface{}) {
	if err == nil {
		p.queue.Forget(key)
		return
	}

	if p.queue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing publicippool %v: %v", key, err)

		p.queue.AddRateLimited(key)
		return
	}

	klog.Infof("Dropping publicippool %q out of the queue: %v", key, err)
	p.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (p *PublicIPPoolController) syncHandler(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	if name != "default-public-ippool" {
		return nil
	}

	ippool, err := p.publicIPPoolLister.Get(name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		// Case DELETE
		// Release allocator's nets
		var emptyString []string
		p.allocator.UpdateIPPool(emptyString)

		return nil
	}

	if p.observedGeneration == ippool.GetGeneration() {
		return nil
	} else if p.observedGeneration > ippool.GetGeneration() {
		klog.Errorf("observedGeneration higher than current Generation, init generation value")
		p.observedGeneration = 0
	}

	// case p.observedGeneration < ippool.GetGeneration()

	err = p.allocator.UpdateIPPool(ippool.Spec.PublicAddresses)
	if err != nil {
		klog.Errorf("error updating allocator ippool: %s", err.Error())
		return err
	}

	// Update status
	ippoolCopy := ippool.DeepCopy()
	ippoolCopy.Status.ObservedGeneration = ippool.GetGeneration()
	err = p.updateStatus(ippoolCopy)
	if err != nil {
		klog.Errorf("error updating status: %s", err.Error())
		return err
	}

	p.observedGeneration = ippool.GetGeneration()
	klog.Infof("default-public-ippool updated: %v (generation %d)", ippool.Spec.PublicAddresses, p.observedGeneration)

	return nil
}

// updateStatus updates status. ippool must be deepcopied object.
func (p *PublicIPPoolController) updateStatus(ippool *v1alpha1.PublicIPPool) error {
	_, err := p.customClientset.NetworkV1alpha1().PublicIPPools().UpdateStatus(context.TODO(), ippool, metav1.UpdateOptions{})
	if err != nil {
		klog.Error("Error update service status: %s", err.Error())
	}

	return err
}
