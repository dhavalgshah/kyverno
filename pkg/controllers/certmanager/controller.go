package certmanager

import (
	"os"
	"reflect"
	"time"

	"github.com/kyverno/kyverno/pkg/common"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/tls"
	v1 "k8s.io/api/core/v1"
	informerv1 "k8s.io/client-go/informers/core/v1"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type Controller interface {
	// Run starts the certManager
	Run(stopCh <-chan struct{})

	// GetTLSPemPair gets the existing TLSPemPair from the secret
	GetTLSPemPair() ([]byte, []byte, error)
}

type controller struct {
	renewer         *tls.CertRenewer
	secretLister    listersv1.SecretLister
	secretQueue     chan bool
	onSecretChanged func() error
}

func NewController(secretInformer informerv1.SecretInformer, certRenewer *tls.CertRenewer, onSecretChanged func() error) (Controller, error) {
	manager := &controller{
		renewer:         certRenewer,
		secretLister:    secretInformer.Lister(),
		secretQueue:     make(chan bool, 1),
		onSecretChanged: onSecretChanged,
	}
	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    manager.addSecretFunc,
		UpdateFunc: manager.updateSecretFunc,
	})
	return manager, nil
}

func (m *controller) addSecretFunc(obj interface{}) {
	secret := obj.(*v1.Secret)
	if secret.GetNamespace() == config.KyvernoNamespace() && secret.GetName() == tls.GenerateTLSPairSecretName() {
		m.secretQueue <- true
	}
}

func (m *controller) updateSecretFunc(oldObj interface{}, newObj interface{}) {
	old := oldObj.(*v1.Secret)
	new := newObj.(*v1.Secret)
	if new.GetNamespace() == config.KyvernoNamespace() && new.GetName() == tls.GenerateTLSPairSecretName() {
		if !reflect.DeepEqual(old.DeepCopy().Data, new.DeepCopy().Data) {
			m.secretQueue <- true
			logger.V(4).Info("secret updated, reconciling webhook configurations")
		}
	}
}

func (m *controller) GetTLSPemPair() ([]byte, []byte, error) {
	secret, err := m.secretLister.Secrets(config.KyvernoNamespace()).Get(tls.GenerateTLSPairSecretName())
	if err != nil {
		return nil, nil, err
	}
	return secret.Data[v1.TLSCertKey], secret.Data[v1.TLSPrivateKeyKey], nil
}

func (m *controller) renewCertificates() error {
	if err := common.RetryFunc(time.Second, 5*time.Second, m.renewer.RenewCA, "failed to renew CA", logger)(); err != nil {
		return err
	}
	if m.onSecretChanged != nil {
		if err := common.RetryFunc(time.Second, 5*time.Second, m.onSecretChanged, "failed to renew CA", logger)(); err != nil {
			return err
		}
	}
	if err := common.RetryFunc(time.Second, 5*time.Second, m.renewer.RenewTLS, "failed to renew TLS", logger)(); err != nil {
		return err
	}
	return nil
}

func (m *controller) GetCAPem() ([]byte, error) {
	secret, err := m.secretLister.Secrets(config.KyvernoNamespace()).Get(tls.GenerateRootCASecretName())
	if err != nil {
		return nil, err
	}
	result := secret.Data[v1.TLSCertKey]
	if len(result) == 0 {
		result = secret.Data[tls.RootCAKey]
	}
	return result, nil
}

func (m *controller) Run(stopCh <-chan struct{}) {
	logger.Info("start managing certificate")
	certsRenewalTicker := time.NewTicker(tls.CertRenewalInterval)
	defer certsRenewalTicker.Stop()
	for {
		select {
		case <-certsRenewalTicker.C:
			if err := m.renewCertificates(); err != nil {
				logger.Error(err, "unable to renew certificates, force restarting")
				os.Exit(1)
			}
		case <-m.secretQueue:
			if err := m.renewCertificates(); err != nil {
				logger.Error(err, "unable to renew certificates, force restarting")
				os.Exit(1)
			}
		case <-stopCh:
			logger.V(2).Info("stopping cert renewer")
			return
		}
	}
}
