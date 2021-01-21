/*
Copyright 2020 Authors of Arktos.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dispatcher

import (
	"fmt"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	"k8s.io/kubernetes/globalscheduler/controllers/util/openstack"
	clusterclientset "k8s.io/kubernetes/globalscheduler/pkg/apis/cluster/client/clientset/versioned"
	dispatcherclientset "k8s.io/kubernetes/globalscheduler/pkg/apis/dispatcher/client/clientset/versioned"
	dispatcherv1 "k8s.io/kubernetes/globalscheduler/pkg/apis/dispatcher/v1"
	"os"
	"reflect"
	"syscall"
	"time"
)

const dispatcherName = "dispatcher"

var TotalCreateLatency int64 = 0
var TotalDeleteLatency int64 = 0
var TotalPodCreateNum = 0
var TotalPodDeleteNum = 0

type Process struct {
	namespace           string
	name                string
	dispatcherClientset *dispatcherclientset.Clientset
	clusterclientset    *clusterclientset.Clientset
	clientset           *kubernetes.Clientset
	podQueue            chan *v1.Pod
	clusterIpMap        map[string]string
	tokenMap            map[string]string
	clusterRange        dispatcherv1.DispatcherRange
	pid                 int
}

func NewProcess(config *rest.Config, namespace string, name string, quit chan struct{}) Process {
	podQueue := make(chan *v1.Pod, 300)

	dispatcherClientset, err := dispatcherclientset.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	dispatcher, err := dispatcherClientset.GlobalschedulerV1().Dispatchers(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		klog.Fatal(err)
	}

	clusterClientset, err := clusterclientset.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	return Process{
		namespace:           namespace,
		name:                name,
		clientset:           clientset,
		clusterclientset:    clusterClientset,
		dispatcherClientset: dispatcherClientset,
		podQueue:            podQueue,
		clusterIpMap:        make(map[string]string),
		tokenMap:            make(map[string]string),
		pid:                 os.Getgid(),
		clusterRange:        dispatcher.Spec.ClusterRange,
	}
}

func (p *Process) Run(quit chan struct{}) {
	dispatcherSelector := fields.ParseSelectorOrDie("metadata.name=" + p.name)
	dispatcherLW := cache.NewListWatchFromClient(p.dispatcherClientset.GlobalschedulerV1(), "dispatchers", p.namespace, dispatcherSelector)

	dispatcherInformer := cache.NewSharedIndexInformer(dispatcherLW, &dispatcherv1.Dispatcher{}, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	dispatcherInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			if err := syscall.Kill(-p.pid, 15); err != nil {
				klog.Fatalf("Fail to exit the current process %v\n", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			newDispatcher, ok := new.(*dispatcherv1.Dispatcher)
			if !ok {
				klog.Warningf("Failed to convert a new object  %+v to a dispatcher", new)
				return
			}
			if !reflect.DeepEqual(p.clusterRange, newDispatcher.Spec.ClusterRange) {
				p.clusterRange = newDispatcher.Spec.ClusterRange
				if err := syscall.Exec(os.Args[0], os.Args, os.Environ()); err != nil {
					klog.Fatal(err)
				}
			}
		},
	})

	go dispatcherInformer.Run(quit)
	boundPodnformer := p.initPodInformer(v1.PodBound, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				klog.Warningf("Failed to convert an added object  %+v to a pod", obj)
				return
			}
			klog.V(4).Infof("Pod %s with cluster %s has been added", pod.Name, pod.Spec.ClusterName)
			go func() {
				p.podQueue <- pod
			}()
		},
	})
	go boundPodnformer.Run(quit)
	scheduledPodnformer := p.initPodInformer(v1.ClusterScheduled, cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				klog.Warningf("Failed to convert an deleted object  %+v to a pod", obj)
				return
			}
			klog.V(4).Infof("Pod %s with cluster %s has been deleted", pod.Name, pod.Spec.ClusterName)
			go func() {
				p.podQueue <- pod
			}()
		},
	})
	go scheduledPodnformer.Run(quit)
	wait.Until(p.SendPodToCluster, 0, quit)
}

func (p *Process) initPodInformer(phase v1.PodPhase, funcs cache.ResourceEventHandlerFuncs) cache.SharedIndexInformer {
	podSelector := fields.ParseSelectorOrDie(fmt.Sprintf("status.phase=%s,spec.clusterName=gte:%s,spec.clusterName=lte:%s", string(phase),
		p.clusterRange.Start, p.clusterRange.End))
	lw := cache.NewListWatchFromClient(p.clientset.CoreV1(), string(v1.ResourcePods), metav1.NamespaceAll, podSelector)
	podInformer := cache.NewSharedIndexInformer(lw, &v1.Pod{}, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	podInformer.AddEventHandler(funcs)
	return podInformer
}

func (p *Process) SendPodToCluster() {

	pod := <-p.podQueue
	if pod != nil {
		klog.V(3).Infof("Processing the item %v", pod)
		host, err := p.getHostIP(pod.Spec.ClusterName)
		if err != nil {
			klog.Warningf("Failed to get host from the cluster %v", pod.Spec.ClusterName)
			return
		}
		token, err := p.getToken(host)
		if err != nil {
			klog.Warningf("Failed to get token from host %v", host)
			return
		}
		if pod.ObjectMeta.DeletionTimestamp != nil {
			err = openstack.DeleteInstance(host, token, pod.Status.ClusterInstanceId)
			if err == nil {
				klog.V(3).Infof("Deleting request for pod %v has been sent to %v", pod.ObjectMeta.Name, host)
				TotalPodDeleteNum += 1

				// Calculate delete latency
				podDeleteTime := pod.DeletionTimestamp
				currentTime := time.Now().UTC()
				duration := currentTime.Unix() - podDeleteTime.Unix()
				TotalDeleteLatency += duration
				deleteLatency := int(duration)
				klog.V(3).Infof("************************************ Pod Name: %s, Delete Latency: %d second ************************************", pod.Name, deleteLatency)

			} else {
				klog.Warningf("Failed to delete the pod %v with error %v", pod.ObjectMeta.Name, err)
			}

			// Calculate average delete latency
			averageDeleteLatency := int(TotalDeleteLatency) / TotalPodDeleteNum
			klog.V(3).Infof("%%%%%%%%%%%%%%%%%%%%%%%%%% Pod Number: %d, Average Delete Latency: %d second %%%%%%%%%%%%%%%%%%%%%%%%%%", TotalPodNum, averageDeleteLatency)
		} else {
			instanceId, err := openstack.ServerCreate(host, token, &pod.Spec)
			if err == nil {
				klog.V(3).Infof("Creating request for pod %v has been sent to %v", pod.ObjectMeta.Name, host)
				TotalPodCreateNum += 1
				pod.Status.ClusterInstanceId = instanceId
				pod.Status.Phase = v1.ClusterScheduled
				updatedPod, err := p.clientset.CoreV1().Pods(pod.ObjectMeta.Namespace).UpdateStatus(pod)
				if err == nil {
					klog.V(3).Infof("Creating request for pod %v returned successfully with %v", updatedPod, instanceId)

					// Calculate create latency
					podCreateTime := pod.CreationTimestamp
					currentTime := time.Now().UTC()
					duration := currentTime.Unix() - podCreateTime.Unix()
					TotalCreateLatency += duration
					createLatency := int(duration)
					klog.V(3).Infof("************************************ Pod Name: %s, Create Latency: %d second ************************************", pod.Name, createLatency)

				} else {
					klog.Warningf("Failed to update the pod %v with error %v", pod.ObjectMeta.Name, err)
				}

				// Calculate average create latency
				averageCreateLatency := int(TotalCreateLatency) / TotalPodCreateNum
				klog.V(3).Infof("%%%%%%%%%%%%%%%%%%%%%%%%%% Pod Number: %d, Average Create Latency: %d second %%%%%%%%%%%%%%%%%%%%%%%%%%", TotalPodNum, averageCreateLatency)
			} else {
				pod.Status.Phase = v1.PodFailed
				if _, err := p.clientset.CoreV1().Pods(pod.ObjectMeta.Namespace).UpdateStatus(pod); err != nil {
					klog.Warningf("Failed to create the pod %v with error %v", pod.ObjectMeta.Name, err)
				}
			}
		}
	}
}

func (p *Process) getToken(ip string) (string, error) {
	if token, ok := p.tokenMap[ip]; ok {
		if !openstack.TokenExpired(ip, token) {
			return token, nil
		}
	}
	token, err := openstack.RequestToken(ip)
	if err != nil {
		return "", err
	}
	p.tokenMap[ip] = token
	return token, nil

}

func (p *Process) getHostIP(clusterName string) (string, error) {
	if ipAddress, ok := p.clusterIpMap[clusterName]; ok {
		return ipAddress, nil
	}
	cluster, err := p.clusterclientset.GlobalschedulerV1().Clusters(metav1.NamespaceDefault).Get(clusterName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	p.clusterIpMap[clusterName] = cluster.Spec.IpAddress
	return p.clusterIpMap[clusterName], nil
}
