// code referenced from online sources

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes/typed/core/v1"
)

var (
	id            = flag.String("id", uuid.New().String(), " pod's participant ID")
	port          = flag.Int("electionPort", 4040, "leader election notifications to this port")
	lockName      = flag.String("lockName", "", "the lock resource name")
	lockNamespace = flag.String("lockNamespace", "default", "the lock resource namespace")
	leaseDuration = flag.Duration("leaseDuration", 4, "time (seconds) that non-leader candidates will wait to force acquire leadership")
	renewDeadline = flag.Duration("renewDeadline", 2, "time (seconds) that the acting leader will retry refreshing leadership before giving up")
	retryPeriod   = flag.Duration("retryPeriod", 1, "time (seconds) LeaderElector candidates should wait between tries of actions")
	leaderID      string
)

func buildConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	flag.Parse()
	klog.InitFlags(nil)

	if *port <= 0 {
		klog.Fatal("Missing --electionPort flag")
	}
	if *lockName == "" {
		klog.Fatal("Missing --lockName flag.")
	}
	if *lockNamespace == "" {
		klog.Fatal("Missing --lockNamespace flag.")
	}

	config, err := buildConfig()
	if err != nil {
		klog.Fatal(err)
	}
	client := clientset.NewForConfigOrDie(config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		klog.Infof("Received termination, stopping leader election for %s", *id)
		cancel()
	}()

	lock := &resourcelock.ConfigMapLock{
		ConfigMapMeta: metav1.ObjectMeta{
			Name:      *lockName,
			Namespace: *lockNamespace,
		},
		Client: client,
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: *id,
		},
	}

	klog.Infof("Commencing leader election for candidate %s", *id)
	listenerRootURL := fmt.Sprintf("http://localhost:%d", *port)
	startURL := fmt.Sprintf("%s/start", listenerRootURL)
	stopURL := fmt.Sprintf("%s/stop", listenerRootURL)
	klog.Infof("Will notify listener at %s of leader changes", listenerRootURL)

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		ReleaseOnCancel: true,
		LeaseDuration:   *leaseDuration * time.Second,
		RenewDeadline:   *renewDeadline * time.Second,
		RetryPeriod:     *retryPeriod * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// This pod is now the leader! Notify listener
				klog.Infof("Notifying %s started leading", *id)
				resp, err := http.Get(startURL)
				if err != nil {
					klog.Errorf("Failed to notify leader of start: %v", err)
				}
				defer resp.Body.Close()
			},
			OnStoppedLeading: func() {
				// This pod stopped leading! Notify listener
				klog.Infof("Notifying %s stopped leading", *id)
				resp, err := http.Get(stopURL)
				if err != nil && ctx.Err() == nil {
					klog.Errorf("Failed to notify leader of stop: %v", err)
				}
				defer resp.Body.Close()
			},
			OnNewLeader: func(identity string) {
				// The leader has changed
				leaderID = identity
				klog.Infof("%s elected as new leader", identity)
			},
		},
	})

}
