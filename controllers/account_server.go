/*
Copyright 2023.

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

package controllers

import (
	"context"
	"strings"
	"sync"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	natsv1alpha1 "github.com/deinstapel/nats-jwt-operator/api/v1alpha1"
	"github.com/nats-io/nats.go"
)

// NatsAccountServer takes NatsAccount and serves them to a nats server (cluster)
type NatsAccountServer struct {
	client.Client
	Scheme     *runtime.Scheme
	accountMap map[string]string
	nc         *nats.Conn
	alive      chan interface{}
	natsReady  sync.Mutex
}

// Configure TLS for Nats client, if required
type NatsTlsConfig struct {
	ClientCertPath string
	ClientKeyPath string
	CaPath string
}

//+kubebuilder:rbac:groups=nats.deinstapel.de,resources=natsaccounts,verbs=get;list;watch;create;update;patch;delete

func NewAccountServer() *NatsAccountServer {
	server := &NatsAccountServer{
		accountMap: make(map[string]string),
		alive: make(chan interface{}),
		natsReady: sync.Mutex{},
	}

	// Keep this locked until r.nc is set at which point we can unlock it
	server.natsReady.Lock()

	return server
}

func connectToNats(url, credsFile string, tlsConf NatsTlsConfig) (*nats.Conn, error) {
	opts := []nats.Option {
		nats.UserCredentials(credsFile),
	}

	if tlsConf.ClientCertPath != "" && tlsConf.ClientKeyPath != "" {
		opts = append(opts, nats.ClientCert(tlsConf.ClientCertPath, tlsConf.ClientKeyPath))
	}

	if tlsConf.CaPath != "" {
		opts = append(opts, nats.RootCAs(tlsConf.CaPath))
	}

	return nats.Connect(url, opts...)
}

func (r *NatsAccountServer) Run(ctx context.Context, url string, credsFile string, tlsConf NatsTlsConfig) error {
	// Close if the this function exits, as we're probably not alive anymore!
	defer close(r.alive)

	logger := log.FromContext(ctx)
	logger.Info("Connecting to nats", "server", url)
	nc, err := connectToNats(url, credsFile, tlsConf)
	if err != nil {
		return err
	}
	r.nc = nc
	// nc is now visible so we can unlock this and allow health checks
	r.natsReady.Unlock()

	logger.Info("subscribing to account lookup")
	sub, err := nc.Subscribe("$SYS.REQ.ACCOUNT.*.CLAIMS.LOOKUP", func(msg *nats.Msg) {
		accountId := strings.TrimSuffix(strings.TrimPrefix(msg.Subject, "$SYS.REQ.ACCOUNT."), ".CLAIMS.LOOKUP")
		logger.Info("account lookup", "accountId", accountId)

		accountToken := r.accountMap[accountId]

		if err := msg.Respond([]byte(accountToken)); err != nil {
			logger.Info("Failed to respond to NATS with token: %v", err)
		}
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return sub.Unsubscribe()
}

func (r *NatsAccountServer) Healthy(req *http.Request) error {
	if _, ok := <- r.alive ; !ok {
		return fmt.Errorf("Not connected to NATs")
	} else {
		return nil
	}
}

func (r *NatsAccountServer) Ready(req *http.Request) error {
	if !r.natsReady.TryLock() {
		return fmt.Errorf("Not initialised NATs client yet")
	}

	r.natsReady.Unlock()
	if !r.nc.IsConnected() || r.nc.IsReconnecting() {
		return fmt.Errorf("NATs is not connected")
	}

	// All seems good
	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *NatsAccountServer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	account := &natsv1alpha1.NatsAccount{}
	if err := r.Get(ctx, req.NamespacedName, account); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if account.DeletionTimestamp != nil {
		// We're not further processing the deletion here.
		// TODO: correctly handle account revocation
		delete(r.accountMap, account.Status.PublicKey)
		return ctrl.Result{}, nil
	}

	if account.Status.JWT != "" && account.Status.PublicKey != "" {
		r.accountMap[account.Status.PublicKey] = account.Status.JWT

		if r.nc != nil {
			go func() {
				if err := r.nc.Publish("$SYS.REQ.CLAIMS.UPDATE", []byte(account.Status.JWT)); err != nil {
					logger.Info("failed to publish claims update", "account", account.Name, "err", err)
				}
			}()
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NatsAccountServer) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&natsv1alpha1.NatsAccount{}).
		Complete(r)
}
