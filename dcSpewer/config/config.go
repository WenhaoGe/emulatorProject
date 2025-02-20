package config

// Describes the configuration of a Device connector.
// Details the inventory and behavior.
type DeviceConfiguration struct {
	// The asset.Platform type of this device.
	// e.g. UCSFI
	PlatformType string

	// The hostname prefix to register devices with.
	// Optional: If not defined devices connect with pre-defined value
	Hostname string

	// The absolute hostname all connections will register with
	HostnameAbs []string

	// The absolute serial all connections will register with
	Serial []string

	// The nodes of devices within the cluster.
	// If no node is given device will default to a single connection with an empty node id. (e.g. standalone rack)
	Nodes []DeviceNode

	// The URL to the debug trace file for this device. Used if either no node specific trace exists or the
	// lookup to node trace fails.
	// Optional: If not defined will fallback to plugin implementations in emulator.
	DebugTraceLog string

	// The URL to the inventory database file if applicable that can be used for inventory.
	// Optional: If not defined device will respond with empty inventory.
	InventoryDatabase string

	// List of children devices to register under this device.
	Children []DeviceConfiguration

	//Server ports that need to be enabled for dc to start.
	//Each string should be in following format "<slotId>/<portId>", e.g. "1/24"
	Ports []string
}

// Describes the configuration of a node within a cluster of devices.
type DeviceNode struct {
	// The Member Identity of this node
	NodeId string

	// The URL to the debug trace file for this node id.
	// Optional: If not defined will fallback to plugin implementations in emulator.
	DebugTraceLog string
}
