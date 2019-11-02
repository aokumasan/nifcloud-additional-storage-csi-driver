package devicemanager

import (
	"fmt"
	"strings"
	"sync"

	"github.com/alice02/nifcloud-sdk-go-v2/service/computing"
	"github.com/aws/aws-sdk-go-v2/aws"
	"k8s.io/klog"
)

const devPreffix = "/dev/sd"

type Device struct {
	Instance          *computing.InstancesSetItem
	Path              string
	VolumeID          string
	IsAlreadyAssigned bool

	isTainted   bool
	releaseFunc func() error
}

func (d *Device) Release(force bool) {
	if !d.isTainted || force {
		if err := d.releaseFunc(); err != nil {
			klog.Errorf("Error releasing device: %v", err)
		}
	}
}

// Taint marks the device as no longer reusable
func (d *Device) Taint() {
	d.isTainted = true
}

type DeviceManager interface {
	// NewDevice retrieves the device if the device is already assigned.
	// Otherwise it creates a new device with next available device name
	// and mark it as unassigned device.
	NewDevice(instance *computing.InstancesSetItem, volumeID string) (device *Device, err error)

	// GetDevice returns the device already assigned to the volume.
	GetDevice(instance *computing.InstancesSetItem, volumeID string) (device *Device, err error)
}

type deviceManager struct {
	// nameAllocator assigns new device name
	nameAllocator NameAllocator

	// We keep an active list of devices we have assigned but not yet
	// attached, to avoid a race condition where we assign a device mapping
	// and then get a second request before we attach the volume.
	mux      sync.Mutex
	inFlight inFlightAttaching
}

var _ DeviceManager = &deviceManager{}

// inFlightAttaching represents the device names being currently attached to nodes.
// A valid pseudo-representation of it would be {"nodeID": {"deviceName: "volumeID"}}.
type inFlightAttaching map[string]map[string]string

func (i inFlightAttaching) Add(nodeID, volumeID, name string) {
	attaching := i[nodeID]
	if attaching == nil {
		attaching = make(map[string]string)
		i[nodeID] = attaching
	}
	attaching[name] = volumeID
}

func (i inFlightAttaching) Del(nodeID, name string) {
	delete(i[nodeID], name)
}

func (i inFlightAttaching) GetNames(nodeID string) map[string]string {
	return i[nodeID]
}

func (i inFlightAttaching) GetVolume(nodeID, name string) string {
	return i[nodeID][name]
}

func NewDeviceManager() DeviceManager {
	return &deviceManager{
		nameAllocator: &nameAllocator{},
		inFlight:      make(inFlightAttaching),
	}
}

func (d *deviceManager) NewDevice(instance *computing.InstancesSetItem, volumeID string) (*Device, error) {
	d.mux.Lock()
	defer d.mux.Unlock()

	if instance == nil {
		return nil, fmt.Errorf("instance is nil")
	}

	// Get device names being attached and already attached to this instance
	inUse := d.getDeviceNamesInUse(instance)

	// Check if this volume is already assigned a device on this machine
	if path := d.getPath(inUse, volumeID); path != "" {
		return d.newBlockDevice(instance, volumeID, path, true), nil
	}

	nodeID, err := getInstanceID(instance)
	if err != nil {
		return nil, err
	}

	name, err := d.nameAllocator.GetNext(inUse)
	if err != nil {
		return nil, fmt.Errorf("could not get a free device name to assign to node %s", nodeID)
	}

	// Add the chosen device and volume to the "attachments in progress" map
	d.inFlight.Add(nodeID, volumeID, name)

	return d.newBlockDevice(instance, volumeID, name, false), nil
}

func (d *deviceManager) GetDevice(instance *computing.InstancesSetItem, volumeID string) (*Device, error) {
	d.mux.Lock()
	defer d.mux.Unlock()

	inUse := d.getDeviceNamesInUse(instance)

	if path := d.getPath(inUse, volumeID); path != "" {
		return d.newBlockDevice(instance, volumeID, path, true), nil
	}

	return d.newBlockDevice(instance, volumeID, "", false), nil
}

func (d *deviceManager) newBlockDevice(instance *computing.InstancesSetItem, volumeID string, path string, isAlreadyAssigned bool) *Device {
	device := &Device{
		Instance:          instance,
		Path:              path,
		VolumeID:          volumeID,
		IsAlreadyAssigned: isAlreadyAssigned,

		isTainted: false,
	}
	device.releaseFunc = func() error {
		return d.release(device)
	}
	return device
}

func (d *deviceManager) release(device *Device) error {
	nodeID, err := getInstanceID(device.Instance)
	if err != nil {
		return err
	}

	d.mux.Lock()
	defer d.mux.Unlock()

	existingVolumeID := d.inFlight.GetVolume(nodeID, device.Path)
	if len(existingVolumeID) == 0 {
		// Attaching is not in progress, so there's nothing to release
		return nil
	}

	if device.VolumeID != existingVolumeID {
		// This actually can happen, because GetNext combines the inFlightAttaching map with the volumes
		// attached to the instance (as reported by the EC2 API).  So if release comes after
		// a 10 second poll delay, we might as well have had a concurrent request to allocate a mountpoint,
		// which because we allocate sequentially is very likely to get the immediately freed volume.
		return fmt.Errorf("release on device %q assigned to different volume: %q vs %q", device.Path, device.VolumeID, existingVolumeID)
	}

	klog.V(5).Infof("Releasing in-process attachment entry: %v -> volume %s", device.Path, device.VolumeID)
	d.inFlight.Del(nodeID, device.Path)

	return nil
}

// getDeviceNamesInUse returns the device to volume ID mapping
// the mapping includes both already attached and being attached volumes
func (d *deviceManager) getDeviceNamesInUse(instance *computing.InstancesSetItem) map[string]string {
	nodeID := aws.StringValue(instance.InstanceId)
	inUse := map[string]string{}
	for _, blockDevice := range instance.BlockDeviceMapping {
		name := aws.StringValue(blockDevice.DeviceName)
		if strings.HasPrefix(name, "SCSI") {
			klog.Warningf("Unexpected additional storage DeviceName: %q", aws.StringValue(blockDevice.DeviceName))
		}
		inUse[name] = aws.StringValue(blockDevice.Ebs.VolumeId)
	}
	klog.V(4).Infof("getDeviceNamesInUse: after search BlockDeviceMapping: %v", inUse)

	for name, volumeID := range d.inFlight.GetNames(nodeID) {
		inUse[name] = volumeID
	}
	klog.V(4).Infof("getDeviceNamesInUse: after search d.inFlight.GetNames: %v", inUse)

	return inUse
}

func (d *deviceManager) getPath(inUse map[string]string, volumeID string) string {
	for name, volID := range inUse {
		if volumeID == volID {
			return name
		}
	}
	return ""
}

func getInstanceID(instance *computing.InstancesSetItem) (string, error) {
	if instance == nil {
		return "", fmt.Errorf("can't get ID from a nil instance")
	}
	return aws.StringValue(instance.InstanceId), nil
}