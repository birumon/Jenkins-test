package main

import (
	"flag"
	"time"

	"stonelb/pkg/allocator"
	floatingipcontroller "stonelb/pkg/controller/floatingip"
	publicippoolcontroller "stonelb/pkg/controller/publicippool"
	servicecontroller "stonelb/pkg/controller/service"

	customclientset "stonelb/pkg/generated/clientset/versioned"
	custominformers "stonelb/pkg/generated/informers/externalversions"

	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

func main() {
	var kubeconfig string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Absolute path to a kubeconfig. Only required if out-of-cluster.")
	flag.Parse()

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Error InClusterConfig: %s", err.Error())
	}

	// Create kubernetes clientset
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	customClient, err := customclientset.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building custom clientset: %s", err.Error())
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 1800*time.Second)
	customInformerFactory := custominformers.NewSharedInformerFactory(customClient, 1800*time.Second)

	stopCh := make(chan struct{})
	defer close(stopCh)

	publicIPAllocator := allocator.NewPublicIPAllocator()

	svcController := servicecontroller.NewServiceController(
		kubeClient, customClient,
		kubeInformerFactory.Core().V1().Services(),
		customInformerFactory.Network().V1alpha1().PublicIPPools(),
		publicIPAllocator,
	)

	fipController := floatingipcontroller.NewFloatingIPController(
		kubeClient, customClient,
		customInformerFactory.Network().V1alpha1().FloatingIPs(),
		customInformerFactory.Network().V1alpha1().PublicIPPools(),
		publicIPAllocator,
	)

	ippoolController := publicippoolcontroller.NewPublicIPPoolController(
		kubeClient, customClient,
		customInformerFactory.Network().V1alpha1().PublicIPPools(),
		publicIPAllocator,
	)

	kubeInformerFactory.Start(stopCh)
	customInformerFactory.Start(stopCh)

	go svcController.Run(1, stopCh)
	go fipController.Run(1, stopCh)
	go ippoolController.Run(1, stopCh)

	select {}
}
