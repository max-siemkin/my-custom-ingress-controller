package watcher

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/bep/debounce"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	v01 "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

// A Payload is a collection of Kubernetes data loaded by the watcher.
type Payload struct {
	Ingresses       []IngressPayload
	TLSCertificates map[string]*tls.Certificate
}

// An IngressPayload is an ingress + its service ports.
type IngressPayload struct {
	Ingress      *networking.Ingress
	ServicePorts map[string]map[string]int
}

// A Watcher watches for ingresses in the kubernetes cluster
type Watcher struct {
	client        kubernetes.Interface
	onChanges     func(*Payload)
	factory       informers.SharedInformerFactory
	secretLister  v1.SecretLister
	serviceLister v1.ServiceLister
	ingressLister v01.IngressLister
}

// New creates a new Watcher.
func New(client kubernetes.Interface, onChange func(*Payload)) *Watcher {
	w := Watcher{
		client:    client,
		factory:   informers.NewSharedInformerFactory(client, time.Minute),
		onChanges: onChange,
	}
	w.secretLister = w.factory.Core().V1().Secrets().Lister()
	w.serviceLister = w.factory.Core().V1().Services().Lister()
	w.ingressLister = w.factory.Networking().V1().Ingresses().Lister()
	return &w
}
func (w *Watcher) addBackend(ingressPayload *IngressPayload, backend networking.IngressBackend) error {
	svc, err := w.serviceLister.Services(ingressPayload.Ingress.Namespace).Get(backend.Service.Name)
	if err != nil {
		return err
	}
	m := map[string]int{}
	for _, port := range svc.Spec.Ports {
		m[port.Name] = int(port.Port)
	}
	ingressPayload.ServicePorts[svc.Name] = m
	return nil
}

func (w *Watcher) onChange() {
	log := logrus.WithField("Source", "watcher.onChange")
	payload := &Payload{TLSCertificates: make(map[string]*tls.Certificate)}

	ingresses, err := w.ingressLister.List(labels.Everything())
	if err != nil {
		log.WithError(err).Errorf("w.ingressLister.List")
		return
	}

	for _, ingress := range ingresses {
		ingressPayload := IngressPayload{Ingress: ingress, ServicePorts: map[string]map[string]int{}}
		payload.Ingresses = append(payload.Ingresses, ingressPayload)

		if ingress.Spec.DefaultBackend != nil {
			if err := w.addBackend(&ingressPayload, *ingress.Spec.DefaultBackend); err != nil {
				log.WithError(err).Errorf("w.addBackend")
			}
		}

		for _, rule := range ingress.Spec.Rules {
			if rule.HTTP != nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				if err := w.addBackend(&ingressPayload, path.Backend); err != nil {
					log.WithError(err).Errorf("w.addBackend")
				}
			}
		}

		for _, rec := range ingress.Spec.TLS {
			if rec.SecretName != "" {
				secret, err := w.secretLister.Secrets(ingress.Namespace).Get(rec.SecretName)
				if err != nil {
					log.WithError(err).Errorf(`w.secretLister.Secrets("%s").Get("%s")`, ingress.Namespace, rec.SecretName)
					continue
				}

				cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
				if err != nil {
					log.WithError(err).Errorf(`tls.X509KeyPair`)
					continue
				}

				payload.TLSCertificates[rec.SecretName] = &cert
			}
		}
	}

	w.onChanges(payload)
}

// Run runs the watcher.
func (w *Watcher) Run(ctx context.Context) error {
	debounced := debounce.New(time.Second)
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { debounced(w.onChange) },
		UpdateFunc: func(_, _ any) { debounced(w.onChange) },
		DeleteFunc: func(_ any) { debounced(w.onChange) },
	}

	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		informer := w.factory.Core().V1().Secrets().Informer()
		if _, err := informer.AddEventHandler(handler); err != nil {
			return err
		}
		informer.Run(egCtx.Done())
		return nil
	})
	eg.Go(func() error {
		informer := w.factory.Networking().V1().Ingresses().Informer()
		if _, err := informer.AddEventHandler(handler); err != nil {
			return err
		}
		informer.Run(egCtx.Done())
		return nil
	})
	eg.Go(func() error {
		informer := w.factory.Core().V1().Services().Informer()
		if _, err := informer.AddEventHandler(handler); err != nil {
			return err
		}
		informer.Run(ctx.Done())
		return nil
	})
	return eg.Wait()
}
