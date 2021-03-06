/*
 * Copyright 2019 The NATS Authors
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package core

import (
	"fmt"
	"time"

	"github.com/nats-io/nats-replicator/server/conf"
	gnatsserver "github.com/nats-io/nats-server/v2/server"
	gnatsd "github.com/nats-io/nats-server/v2/test"
	nss "github.com/nats-io/nats-streaming-server/server"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	stan "github.com/nats-io/stan.go"
)

const (
	serverCert = "../../resources/certs/server-cert.pem"
	serverKey  = "../../resources/certs/server-key.pem"
	// clientCert = "../../resources/certs/client-cert.pem"
	// clientKey  = "../../resources/certs/client-key.pem"
	caFile = "../../resources/certs/truststore.pem"
)

// TestEnv encapsulate a bridge test environment
type TestEnv struct {
	Config *conf.NATSReplicatorConfig
	Gnatsd *gnatsserver.Server
	Stan   *nss.StanServer

	NC *nats.Conn // for bypassing the bridge
	SC stan.Conn  // for bypassing the bridge

	natsPort       int
	natsURL        string
	clusterName    string
	clientID       string // we keep this so we stay the same on reconnect
	bridgeClientID string

	Bridge *NATSReplicator

	useTLS bool
}

// StartTestEnvironment calls StartTestEnvironmentInfrastructure
// followed by StartReplicator
func StartTestEnvironment(connections []conf.ConnectorConfig) (*TestEnv, error) {
	tbs, err := StartTestEnvironmentInfrastructure(false)
	if err != nil {
		return nil, err
	}
	err = tbs.StartReplicator(connections)
	if err != nil {
		tbs.Close()
		return nil, err
	}
	return tbs, err
}

// StartTLSTestEnvironment calls StartTestEnvironmentInfrastructure
// followed by StartReplicator, with TLS enabled
func StartTLSTestEnvironment(connections []conf.ConnectorConfig) (*TestEnv, error) {
	tbs, err := StartTestEnvironmentInfrastructure(true)
	if err != nil {
		return nil, err
	}
	err = tbs.StartReplicator(connections)
	if err != nil {
		tbs.Close()
		return nil, err
	}
	return tbs, err
}

// StartTestEnvironmentInfrastructure creates the kafka server, Nats and streaming
// but does not start a bridge, you can use StartReplicator to start a bridge afterward
func StartTestEnvironmentInfrastructure(useTLS bool) (*TestEnv, error) {
	tbs := &TestEnv{}
	tbs.useTLS = useTLS

	err := tbs.StartNATSandStan(-1, nuid.Next(), nuid.Next(), nuid.Next())
	if err != nil {
		tbs.Close()
		return nil, err
	}

	return tbs, nil
}

// StartReplicator is the second half of StartTestEnvironment
// it is provided separately so that environment can be created before the bridge runs
func (tbs *TestEnv) StartReplicator(connections []conf.ConnectorConfig) error {
	config := conf.DefaultConfig()
	config.ReconnectInterval = 200
	config.Logging.Debug = true
	config.Logging.Trace = true
	config.Logging.Colors = false
	config.Monitoring = conf.HTTPConfig{
		HTTPPort: -1,
	}
	config.NATS = []conf.NATSConfig{}
	config.STAN = []conf.NATSStreamingConfig{}

	config.NATS = append(config.NATS, conf.NATSConfig{
		Name:           "nats",
		Servers:        []string{tbs.natsURL},
		ConnectTimeout: 2000,
		ReconnectWait:  2000,
		MaxReconnects:  5,
	})
	config.STAN = append(config.STAN, conf.NATSStreamingConfig{
		Name:               "stan",
		ClusterID:          tbs.clusterName,
		ClientID:           tbs.bridgeClientID,
		PubAckWait:         5000,
		DiscoverPrefix:     stan.DefaultDiscoverPrefix,
		MaxPubAcksInflight: stan.DefaultMaxPubAcksInflight,
		ConnectWait:        2000,
		NATSConnection:     "nats",
		PingInterval:       1,
		MaxPings:           3,
	})

	if tbs.useTLS {
		config.Monitoring.HTTPPort = 0
		config.Monitoring.HTTPSPort = -1

		config.Monitoring.TLS = conf.TLSConf{
			Cert: serverCert,
			Key:  serverKey,
		}

		c := config.NATS[0]
		c.TLS = conf.TLSConf{
			Root: caFile,
		}
		config.NATS[0] = c
	}

	config.Connect = connections

	tbs.Config = &config
	tbs.Bridge = NewNATSReplicator()
	err := tbs.Bridge.InitializeFromConfig(config)
	if err != nil {
		tbs.Close()
		return err
	}
	err = tbs.Bridge.Start()
	if err != nil {
		tbs.Close()
		return err
	}

	return nil
}

// StartNATSandStan starts up the nats and stan servers
func (tbs *TestEnv) StartNATSandStan(port int, clusterID string, clientID string, bridgeClientID string) error {
	var err error
	opts := gnatsd.DefaultTestOptions
	opts.Port = port

	if tbs.useTLS {
		opts.TLSCert = serverCert
		opts.TLSKey = serverKey
		opts.TLSTimeout = 5

		tc := gnatsserver.TLSConfigOpts{}
		tc.CertFile = opts.TLSCert
		tc.KeyFile = opts.TLSKey

		opts.TLSConfig, err = gnatsserver.GenTLSConfig(&tc)

		if err != nil {
			return err
		}
	}
	tbs.Gnatsd = gnatsd.RunServer(&opts)

	if tbs.useTLS {
		tbs.natsURL = fmt.Sprintf("tls://localhost:%d", opts.Port)
	} else {
		tbs.natsURL = fmt.Sprintf("nats://localhost:%d", opts.Port)
	}

	tbs.natsPort = opts.Port
	tbs.clusterName = clusterID
	sOpts := nss.GetDefaultOptions()
	sOpts.ID = tbs.clusterName
	sOpts.NATSServerURL = tbs.natsURL

	if tbs.useTLS {
		sOpts.ClientCA = caFile
	}

	nOpts := nss.DefaultNatsServerOptions
	nOpts.Port = -1

	s, err := nss.RunServerWithOpts(sOpts, &nOpts)
	if err != nil {
		return err
	}

	tbs.Stan = s
	tbs.clientID = clientID
	tbs.bridgeClientID = bridgeClientID

	var nc *nats.Conn

	if tbs.useTLS {
		nc, err = nats.Connect(tbs.natsURL, nats.RootCAs(caFile))
	} else {
		nc, err = nats.Connect(tbs.natsURL)
	}

	if err != nil {
		return err
	}

	tbs.NC = nc

	sc, err := stan.Connect(tbs.clusterName, tbs.clientID, stan.NatsConn(tbs.NC))
	if err != nil {
		return err
	}
	tbs.SC = sc

	return nil
}

// StopReplicator stops the bridge
func (tbs *TestEnv) StopReplicator() {
	if tbs.Bridge != nil {
		tbs.Bridge.Stop()
		tbs.Bridge = nil
	}
}

// StopNATS shuts down the NATS and Stan servers
func (tbs *TestEnv) StopNATS() error {
	if tbs.SC != nil {
		tbs.SC.Close()
		tbs.SC = nil
	}

	if tbs.NC != nil {
		tbs.NC.Close()
		tbs.NC = nil
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
		tbs.Stan = nil
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
		tbs.Gnatsd = nil
	}

	return nil
}

// RestartNATS shuts down the NATS and stan server and then starts it again
func (tbs *TestEnv) RestartNATS() error {
	if tbs.SC != nil {
		tbs.SC.Close()
	}

	if tbs.NC != nil {
		tbs.NC.Close()
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
	}

	err := tbs.StartNATSandStan(tbs.natsPort, tbs.clusterName, tbs.clientID, tbs.bridgeClientID)
	if err != nil {
		return err
	}

	return nil
}

// Close the bridge server and clean up the test environment
func (tbs *TestEnv) Close() {
	// Stop the bridge first!
	if tbs.Bridge != nil {
		tbs.Bridge.Stop()
	}

	if tbs.SC != nil {
		tbs.SC.Close()
	}

	if tbs.NC != nil {
		tbs.NC.Close()
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
	}
}

func (tbs *TestEnv) WaitForIt(requestCount int64, done chan string) string {
	timeout := time.Duration(5000) * time.Millisecond // 5 second timeout for tests
	stop := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	requestsOk := make(chan bool)

	// Timeout the done channel
	go func() {
		<-timer.C
		done <- ""
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	go func() {
		for t := range ticker.C {
			if t.After(stop) {
				requestsOk <- false
				break
			}

			if tbs.Bridge.SafeStats().RequestCount >= requestCount {
				requestsOk <- true
				break
			}
		}
		ticker.Stop()
	}()

	received := <-done
	ok := <-requestsOk

	if !ok {
		received = ""
	}

	return received
}

func (tbs *TestEnv) WaitForRequests(requestCount int64) {
	timeout := time.Duration(5000) * time.Millisecond // 5 second timeout for tests
	stop := time.Now().Add(timeout)
	requestsOk := make(chan bool)

	ticker := time.NewTicker(50 * time.Millisecond)
	go func() {
		for t := range ticker.C {
			if t.After(stop) {
				requestsOk <- false
				break
			}

			if tbs.Bridge.SafeStats().RequestCount >= requestCount {
				requestsOk <- true
				break
			}
		}
		ticker.Stop()
	}()

	<-requestsOk
}
