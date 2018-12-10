/*
 * Copyright (C) 2018 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package connection

import (
	"sync"

	log "github.com/cihub/seelog"
	"github.com/mysteriumnetwork/node/core/connection"
	wg "github.com/mysteriumnetwork/node/services/wireguard"
	endpoint "github.com/mysteriumnetwork/node/services/wireguard/endpoint"
)

const logPrefix = "[connection-wireguard] "

// Connection which does wireguard tunneling.
type Connection struct {
	connection   sync.WaitGroup
	stateChannel connection.StateChannel

	config             wg.ServiceConfig
	consumerKey        wg.ConsumerPrivateKey
	connectionEndpoint wg.ConnectionEndpoint
}

// Start establish wireguard connection to the service provider.
func (c *Connection) Start() (err error) {
	c.connectionEndpoint, err = endpoint.NewConnectionEndpoint(nil)
	if err != nil {
		return err
	}

	c.connection.Add(1)
	c.stateChannel <- connection.Connecting

	if err := c.connectionEndpoint.Start(&c.config); err != nil {
		c.stateChannel <- connection.NotConnected
		c.connection.Done()
		return err
	}

	if err := c.connectionEndpoint.AddPeer(c.config.Provider.PublicKey, &c.config.Provider.Endpoint); err != nil {
		c.stateChannel <- connection.NotConnected
		c.connection.Done()
		return err
	}
	c.stateChannel <- connection.Connected
	return nil
}

// Wait blocks until wireguard connection not stopped.
func (c *Connection) Wait() error {
	c.connection.Wait()
	return nil
}

// Stop stops wireguard connection and closes connection endpoint.
func (c *Connection) Stop() {
	c.stateChannel <- connection.Disconnecting

	if err := c.connectionEndpoint.Stop(); err != nil {
		log.Error(logPrefix, "Failed to close wireguard connection", err)
	}

	c.stateChannel <- connection.NotConnected
	c.connection.Done()
	close(c.stateChannel)
}

// GenerateConnectionParams generates the wg specific connection params
func GenerateConnectionParams() (cPubKey wg.ConsumerPublicKey, cPrivKey wg.ConsumerPrivateKey, err error) {
	privateKey, err := endpoint.GeneratePrivateKey()
	if err != nil {
		return cPubKey, cPrivKey, err
	}
	publicKey, err := endpoint.PrivateKeyToPublicKey(privateKey)
	if err != nil {
		return cPubKey, cPrivKey, err
	}
	return wg.ConsumerPublicKey{
			PublicKey: publicKey,
		}, wg.ConsumerPrivateKey{
			PrivateKey: privateKey,
		}, nil
}