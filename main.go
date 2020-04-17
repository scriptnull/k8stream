package main

import (
	fmt "fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

var (
	configFile = kingpin.Flag("config", "Config File to Parse").Required().File()
)

const (
	VERSION = "0.0.1"
)

func main() {
	kingpin.Version(VERSION)
	kingpin.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	cData, err := readConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	conf := &L9K8streamConfig{}
	if err := loadConfig(cData, conf); err != nil {
		log.Fatal(err)
	}

	kc, err := newK8sClient(conf.KubeConfig)
	if err != nil {
		log.Fatal(err)
	}

	factory := informers.NewSharedInformerFactory(kc.Clientset, time.Duration(60)*time.Second)
	informer := factory.Core().V1().Events().Informer()

	mcache, err := cacheClient()
	if err != nil {
		log.Fatal(err)
	}

	sink, err := getFlusher(conf, cData)
	if err != nil {
		log.Fatal(err)
	}

	ch := NewBatch(
		conf.UID, conf.BatchSize, conf.BatchInterval, sink, mcache,
	)

	h := &Handler{kc, ch, mcache}

	stopCh := make(chan struct{})
	informer.AddEventHandler(h)
	go informer.Run(stopCh)

	if err := StartHeartbeat(conf.UID, conf.HeartbeatHook, conf.HeartbeatInterval, conf.HeartbeatTimeout); err != nil {
		log.Fatal(err)
	}

	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	sigCh := make(chan os.Signal, 0)
	signal.Notify(sigCh, os.Kill, os.Interrupt, syscall.SIGQUIT)

	i := <-sigCh
	close(stopCh)

	if i == syscall.SIGQUIT {
		time.Sleep(300 * time.Millisecond)
		os.Exit(1)
	}
}
