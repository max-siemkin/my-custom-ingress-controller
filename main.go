package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/max-siemkin/my-custom-ingress-controller/server"
	"github.com/max-siemkin/my-custom-ingress-controller/watcher"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	viper.AutomaticEnv()

	// LOGS init
	logrus.SetFormatter(&logrus.JSONFormatter{})
	logrus.SetOutput(os.Stdout)
	logrus.SetReportCaller(true)
	level, err := logrus.ParseLevel(viper.GetString("LOG_LEVEL"))
	if err != nil {
		logrus.WithError(err).Fatal("logrus.ParseLevel")
	}
	logrus.SetLevel(level)
	log := logrus.WithField("Source", "main func")

	// K8s config
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
	}
	if err != nil {
		log.Fatal(err)
	}

	// K8s client
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	// Init server
	s := server.New()

	// Init watcher
	w := watcher.New(client, func(payload *watcher.Payload) { s.Update(payload) })

	// Start
	ctx, cancel := context.WithCancel(context.Background())
	eg, egCtx := errgroup.WithContext(ctx) // это означает, что всякий раз, когда родительский контекст отменяется, дочерний также отменяется
	// Контекст, возвращаемый функцией WithContext(egCtx), также отменяется, когда любая из функций, запущенных в группе ошибок, завершается с ошибкой.

	eg.Go(func() error {
		// Graceful shutdown
		defer cancel()
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
		<-done
		return nil
	})
	eg.Go(func() error { return w.Run(egCtx) })
	eg.Go(func() error { return s.Run(egCtx) })
	if err := eg.Wait(); err != nil {
		log.Fatal(err)
	}
}
