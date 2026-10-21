package vsphere

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// PowerActions are the VM power operations Apex RMM exposes.
var PowerActions = map[string]string{
	"on":       "Power on",
	"shutdown": "Shut down guest",
	"reboot":   "Restart guest",
	"off":      "Power off (hard)",
	"reset":    "Reset (hard)",
	"suspend":  "Suspend",
}

// PowerVM runs a power operation and waits for it to finish. "shutdown" and
// "reboot" are graceful and need VMware Tools running in the guest.
func (c *Conn) PowerVM(ctx context.Context, ref, action string) error {
	if _, ok := PowerActions[action]; !ok {
		return fmt.Errorf("unknown power action %q", action)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	vm := object.NewVirtualMachine(c.c.Client, types.ManagedObjectReference{Type: "VirtualMachine", Value: ref})

	var task *object.Task
	var err error
	switch action {
	case "on":
		task, err = vm.PowerOn(ctx)
	case "off":
		task, err = vm.PowerOff(ctx)
	case "reset":
		task, err = vm.Reset(ctx)
	case "suspend":
		task, err = vm.Suspend(ctx)
	case "shutdown":
		err = vm.ShutdownGuest(ctx)
	case "reboot":
		err = vm.RebootGuest(ctx)
	}
	if err == nil && task != nil {
		err = task.Wait(ctx)
	}
	return powerErr(err)
}

func powerErr(err error) error {
	if err == nil {
		return nil
	}
	var fault types.BaseMethodFault
	var te task.Error
	switch {
	case errors.As(err, &te):
		fault = te.Fault()
	case soap.IsVimFault(err):
		fault = soap.ToVimFault(err)
	}
	switch f := fault.(type) {
	case *types.InvalidPowerState:
		return fmt.Errorf("the VM is %s, so that action does not apply", stateWord(string(f.ExistingState)))
	case *types.ToolsUnavailable:
		return errors.New("VMware Tools is not running in the guest, so a graceful action is not possible; use the hard power action instead")
	case *types.NoPermission:
		return errors.New("the vCenter account does not have permission for that action")
	}
	return cleanErr(err)
}

func stateWord(s string) string {
	switch s {
	case "poweredOn":
		return "powered on"
	case "poweredOff":
		return "powered off"
	case "suspended":
		return "suspended"
	}
	return s
}

// VMLink returns a URL that opens the VM in the vSphere Client (vCenter) or the
// ESXi Host Client, from where the web or remote console can be launched.
func VMLink(endpoint *url.URL, about About, ref string) string {
	base := (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host}).String()
	if about.APIType == "VirtualCenter" {
		return base + "/ui/app/vm;nav=v/urn:vmomi:VirtualMachine:" + ref + ":" + about.InstanceUUID + "/summary"
	}
	return base + "/ui/#/host/vms/" + strings.TrimPrefix(ref, "vm-")
}

// HostLink returns a URL that opens the host in its management UI.
func HostLink(endpoint *url.URL, about About, ref string) string {
	base := (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host}).String()
	if about.APIType == "VirtualCenter" {
		return base + "/ui/app/host;nav=h/urn:vmomi:HostSystem:" + ref + ":" + about.InstanceUUID + "/summary"
	}
	return base + "/ui/"
}
