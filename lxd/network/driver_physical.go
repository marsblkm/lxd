package network

import (
	"fmt"
	"net"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	dbCluster "github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/validate"
)

// physical represents a LXD physical network.
type physical struct {
	common
}

// Type returns the network type.
func (n *physical) Type() string {
	return "physical"
}

// DBType returns the network type DB ID.
func (n *physical) DBType() db.NetworkType {
	return db.NetworkTypePhysical
}

// Validate network config.
func (n *physical) Validate(config map[string]string) error {
	rules := map[string]func(value string) error{
		"parent":                      validate.Required(validate.IsNotEmpty, validate.IsInterfaceName),
		"mtu":                         validate.Optional(validate.IsNetworkMTU),
		"vlan":                        validate.Optional(validate.IsNetworkVLAN),
		"gvrp":                        validate.Optional(validate.IsBool),
		"maas.subnet.ipv4":            validate.IsAny,
		"maas.subnet.ipv6":            validate.IsAny,
		"ipv4.gateway":                validate.Optional(validate.IsNetworkAddressCIDRV4),
		"ipv6.gateway":                validate.Optional(validate.IsNetworkAddressCIDRV6),
		"ipv4.ovn.ranges":             validate.Optional(validate.IsNetworkRangeV4List),
		"ipv6.ovn.ranges":             validate.Optional(validate.IsNetworkRangeV6List),
		"ipv4.routes":                 validate.Optional(validate.IsNetworkV4List),
		"ipv4.routes.anycast":         validate.Optional(validate.IsBool),
		"ipv6.routes":                 validate.Optional(validate.IsNetworkV6List),
		"ipv6.routes.anycast":         validate.Optional(validate.IsBool),
		"dns.nameservers":             validate.Optional(validate.IsNetworkAddressList),
		"ovn.ingress_mode":            validate.Optional(validate.IsOneOf("l2proxy", "routed")),
		"volatile.last_state.created": validate.Optional(validate.IsBool),
	}

	err := n.validate(config, rules)
	if err != nil {
		return err
	}

	return nil
}

// checkParentUse checks if parent is already in use by another network or instance device.
func (n *physical) checkParentUse(ourConfig map[string]string) (bool, error) {
	// Get all managed networks across all projects.
	var err error
	var projectNetworks map[string]map[int64]api.Network

	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNetworks, err = tx.GetCreatedNetworks()
		return err
	})
	if err != nil {
		return false, errors.Wrapf(err, "Failed to load all networks")
	}

	for projectName, networks := range projectNetworks {
		if projectName != project.Default {
			continue // Only default project networks can possibly reference a physical interface.
		}

		for _, network := range networks {
			if network.Name == n.name {
				continue // Ignore our own DB record.
			}

			// Check if another network is using our parent.
			if network.Config["parent"] == ourConfig["parent"] {
				// If either network doesn't specify a vlan, or both specify same vlan,
				// then we can't use this parent.
				if (network.Config["vlan"] == "" || ourConfig["vlan"] == "") || network.Config["vlan"] == ourConfig["vlan"] {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// Create checks whether the referenced parent interface is used by other networks or instance devices, as we
// need to have exclusive access to the interface.
func (n *physical) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", log.Ctx{"clientType": clientType, "config": n.config})

	// We only need to check in the database once, not on every clustered node.
	if clientType == request.ClientTypeNormal {
		inUse, err := n.checkParentUse(n.config)
		if err != nil {
			return err
		}
		if inUse {
			return fmt.Errorf("Parent interface %q in use by another network", n.config["parent"])
		}
	}

	return nil
}

// Delete deletes a network.
func (n *physical) Delete(clientType request.ClientType) error {
	n.logger.Debug("Delete", log.Ctx{"clientType": clientType})

	err := n.Stop()
	if err != nil {
		return err
	}

	return n.common.delete(clientType)
}

// Rename renames a network.
func (n *physical) Rename(newName string) error {
	n.logger.Debug("Rename", log.Ctx{"newName": newName})

	// Rename common steps.
	err := n.common.rename(newName)
	if err != nil {
		return err
	}

	return nil
}

// Start starts is a no-op.
func (n *physical) Start() error {
	err := n.start()
	if err != nil {
		err := n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningNetworkStartupFailure, err.Error())
		if err != nil {
			n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
		}
	} else {
		err := warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningNetworkStartupFailure, dbCluster.TypeNetwork, int(n.id))
		if err != nil {
			n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
		}
	}

	return err
}

func (n *physical) start() error {
	n.logger.Debug("Start")

	revert := revert.New()
	defer revert.Fail()

	hostName := GetHostDevice(n.config["parent"], n.config["vlan"])
	created, err := VLANInterfaceCreate(n.config["parent"], hostName, n.config["vlan"], shared.IsTrue(n.config["gvrp"]))
	if err != nil {
		return err
	}
	if created {
		revert.Add(func() { InterfaceRemove(hostName) })
	}

	// Set the MTU.
	if n.config["mtu"] != "" {
		phyLink := &ip.Link{Name: hostName}
		err = phyLink.SetMTU(n.config["mtu"])
		if err != nil {
			return errors.Wrapf(err, "Failed setting MTU %q on %q", n.config["mtu"], phyLink.Name)
		}
	}

	// Record if we created this device or not (if we have not already recorded that we created it previously),
	// so it can be removed on stop. This way we won't overwrite the setting on LXD restart.
	if !shared.IsTrue(n.config["volatile.last_state.created"]) {
		n.config["volatile.last_state.created"] = fmt.Sprintf("%t", created)
		err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.UpdateNetwork(n.id, n.description, n.config)
		})
		if err != nil {
			return errors.Wrapf(err, "Failed saving volatile config")
		}
	}

	revert.Success()
	return nil
}

// Stop stops is a no-op.
func (n *physical) Stop() error {
	n.logger.Debug("Stop")

	hostName := GetHostDevice(n.config["parent"], n.config["vlan"])

	// Only try and remove created VLAN interfaces.
	if n.config["vlan"] != "" && shared.IsTrue(n.config["volatile.last_state.created"]) && InterfaceExists(hostName) {
		err := InterfaceRemove(hostName)
		if err != nil {
			return err
		}
	}

	// Reset MTU back to 1500 if overridden in config.
	if n.config["mtu"] != "" && InterfaceExists(hostName) {
		resetMTU := "1500"
		link := &ip.Link{Name: hostName}
		err := link.SetMTU(resetMTU)
		if err != nil {
			return errors.Wrapf(err, "Failed setting MTU %q on %q", link, link.Name)
		}
	}

	// Remove last state config.
	delete(n.config, "volatile.last_state.created")
	err := n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.UpdateNetwork(n.id, n.description, n.config)
	})
	if err != nil {
		return errors.Wrapf(err, "Failed removing volatile config")
	}

	return nil
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *physical) Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	n.logger.Debug("Update", log.Ctx{"clientType": clientType, "newNetwork": newNetwork})

	dbUpdateNeeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeeded {
		return nil // Nothing changed.
	}

	// If the network as a whole has not had any previous creation attempts, or the node itself is still
	// pending, then don't apply the new settings to the node, just to the database record (ready for the
	// actual global create request to be initiated).
	if n.Status() == api.NetworkStatusPending || n.LocalStatus() == api.NetworkStatusPending {
		return n.common.update(newNetwork, targetNode, clientType)
	}

	revert := revert.New()
	defer revert.Fail()

	hostNameChanged := shared.StringInSlice("vlan", changedKeys) || shared.StringInSlice("parent", changedKeys)

	// We only need to check in the database once, not on every clustered node.
	if clientType == request.ClientTypeNormal {
		if hostNameChanged {
			isUsed, err := n.IsUsed()
			if isUsed || err != nil {
				return fmt.Errorf("Cannot update network parent interface when in use")
			}

			inUse, err := n.checkParentUse(newNetwork.Config)
			if err != nil {
				return err
			}
			if inUse {
				return fmt.Errorf("Parent interface %q in use by another network", newNetwork.Config["parent"])
			}
		}
	}

	if hostNameChanged {
		err = n.Stop()
		if err != nil {
			return err
		}

		// Remove the volatile last state from submitted new config if present.
		delete(newNetwork.Config, "volatile.last_state.created")
	}

	// Define a function which reverts everything.
	revert.Add(func() {
		// Reset changes to all nodes and database.
		n.common.update(oldNetwork, targetNode, clientType)
	})

	// Apply changes to all nodes and databse.
	err = n.common.update(newNetwork, targetNode, clientType)
	if err != nil {
		return err
	}

	err = n.Start()
	if err != nil {
		return err
	}

	revert.Success()

	// Notify dependent networks (those using this network as their uplink) of the changes.
	// Do this after the network has been successfully updated so that a failure to notify a dependent network
	// doesn't prevent the network itself from being updated.
	if clientType == request.ClientTypeNormal && len(changedKeys) > 0 {
		n.common.notifyDependentNetworks(changedKeys)
	}

	return nil
}

// DHCPv4Subnet returns the DHCPv4 subnet (if DHCP is enabled on network).
func (n *physical) DHCPv4Subnet() *net.IPNet {
	_, subnet, err := net.ParseCIDR(n.config["ipv4.gateway"])
	if err != nil {
		return nil
	}

	return subnet
}

// DHCPv6Subnet returns the DHCPv6 subnet (if DHCP or SLAAC is enabled on network).
func (n *physical) DHCPv6Subnet() *net.IPNet {
	_, subnet, err := net.ParseCIDR(n.config["ipv6.gateway"])
	if err != nil {
		return nil
	}

	return subnet
}
