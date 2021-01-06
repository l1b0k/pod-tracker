/*
Copyright 2021 l1b0k

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

package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/imdario/mergo"
	log "github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeConfig string

	db *gorm.DB

	cs            *kubernetes.Clientset
	eventInformer cache.SharedIndexInformer
)

func main() {
	db = initDB()
	initK8SClient()

	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0)
	watchPodEvents(factory)

	factory.Start(wait.NeverStop)
	cache.WaitForCacheSync(wait.NeverStop, eventInformer.HasSynced)

	select {}
}

type Pod struct {
	UID       string `gorm:"primaryKey;column:uid;index:idx_uid"`
	Namespace string `gorm:"column:namespace"`
	Name      string `gorm:"column:name"`

	Scheduled      time.Time `gorm:"column:scheduled"`
	AllocIPSucceed time.Time `gorm:"column:alloc_ip_success"`
	Pulling        time.Time `gorm:"column:pulling"`
	Pulled         time.Time `gorm:"column:pulled"`
	Created        time.Time `gorm:"column:created"`
	Started        time.Time `gorm:"column:started"`
	Killing        time.Time `gorm:"column:killing"`
}

func (Pod) TableName() string {
	return "pod"
}

func initDB() *gorm.DB {
	db, err := gorm.Open(sqlite.Open("store.sqlite"), &gorm.Config{Logger: logger.Discard})
	//db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		panic("failed to connect database")
	}
	//db.Debug()
	err = db.AutoMigrate(&Pod{})
	if err != nil {
		panic("failed to migrate database")
	}
	return db
}

func initK8SClient() {
	if home := os.Getenv("HOME"); home != "" {
		flag.StringVar(&kubeConfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) Absolute path to the kubeconfig file")
	} else {
		flag.StringVar(&kubeConfig, "kubeconfig", "", "Absolute path to the kubeconfig file")
	}

	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		panic(err)
	}
	cs, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
}

func watchPodEvents(factory informers.SharedInformerFactory) {
	eventInformer = factory.Core().V1().Events().Informer()
	eventInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: filterPodOnly,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: eventAdd,
		},
	})
}

func filterPodOnly(obj interface{}) bool {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return false
	}
	if e.InvolvedObject.Kind == "Pod" {
		return true
	}
	return false
}

func eventAdd(obj interface{}) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	p := eventToPod(e)
	if p == nil {
		return
	}
	oldPod := Pod{UID: p.UID}

	result := db.First(&oldPod)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		db.Create(p)
		return
	}
	err := mergo.Merge(&oldPod, p, mergo.WithTransformers(timeTransformer{}))
	if err != nil {
		log.Error(err)
		return
	}
	db.Save(&oldPod)
}

func eventToPod(e *corev1.Event) *Pod {
	p := &Pod{
		UID:       string(e.InvolvedObject.UID),
		Namespace: e.InvolvedObject.Namespace,
		Name:      e.InvolvedObject.Name,
	}
	firstOccured := e.CreationTimestamp
	switch e.Reason {
	case "Scheduled":
		p.Scheduled = firstOccured.Time
	case "AllocIPSucceed":
		p.AllocIPSucceed = firstOccured.Time
	case "Pulling":
		p.Pulling = firstOccured.Time
	case "Pulled":
		p.Pulled = firstOccured.Time
	case "Created":
		p.Created = firstOccured.Time
	case "Started":
		p.Started = firstOccured.Time
	case "Killing":
		p.Killing = firstOccured.Time
	default:
		log.Debugf("event %s not supported", e.Reason)
		return nil
	}
	return p
}

type timeTransformer struct {
}

func (t timeTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ == reflect.TypeOf(time.Time{}) {
		return func(dst, src reflect.Value) error {
			if dst.CanSet() {
				isZero := dst.MethodByName("IsZero")
				result := isZero.Call([]reflect.Value{})
				if result[0].Bool() {
					dst.Set(src)
				}
			}
			return nil
		}
	}
	return nil
}
