package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	wtoapi "github.com/jeffdyoung/wto/api/v1alpha1"
	wtocontroller "github.com/jeffdyoung/wto/internal/controller"
	wtowebhook "github.com/jeffdyoung/wto/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(wtoapi.AddToScheme(scheme))
}

func main() {
	var certDir string
	var metricsAddr string
	var webhookPort int
	flag.StringVar(&certDir, "cert-dir", "/tmp/tls", "TLS certificate directory")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Metrics bind address")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Webhook server port")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("wto")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: certDir,
		}),
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&wtocontroller.PlacementReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("wto"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create placement controller")
		os.Exit(1)
	}

	if err := (&wtocontroller.ProfileReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("wto-profile"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create profile controller")
		os.Exit(1)
	}

	mgr.GetWebhookServer().Register("/mutate-pods", &webhook.Admission{
		Handler: &wtowebhook.PodMutatingWebhook{
			Client:  mgr.GetClient(),
			Decoder: admission.NewDecoder(scheme),
		},
	})

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
