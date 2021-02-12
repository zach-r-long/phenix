package mm

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"phenix/internal/common"
	"phenix/internal/mm/mmcli"
)

var (
	ErrCaptureExists     = fmt.Errorf("capture already exists")
	ErrNoCaptures        = fmt.Errorf("no captures exist")
	ErrC2ClientNotActive = fmt.Errorf("C2 client not active for VM")
)

// Mutex to protect minimega cc filter setting when configuring cc commands from
// different Goroutines. This is at the package level to protect across multiple
// instances of the Minimega struct.
var ccMu sync.Mutex

type Minimega struct{}

func (Minimega) ReadScriptFromFile(filename string) error {
	cmd := mmcli.NewCommand()
	cmd.Command = "read " + filename

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("reading mmcli script: %w", err)
	}

	return nil
}

func (Minimega) ClearNamespace(ns string) error {
	cmd := mmcli.NewCommand()
	cmd.Command = "clear namespace " + ns

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("clearing minimega namespace: %w", err)
	}

	return nil
}

func (Minimega) LaunchVMs(ns string) error {
	cmd := mmcli.NewNamespacedCommand(ns)
	cmd.Command = "vm launch"

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("launching VMs: %w", err)
	}

	cmd.Command = "vm start all"

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("starting VMs: %w", err)
	}

	return nil
}

func (Minimega) GetLaunchProgress(ns string, expected int) (float64, error) {
	var queued int

	cmd := mmcli.NewNamespacedCommand(ns)
	cmd.Command = "ns queue"

	re := regexp.MustCompile(`Names: (.*)`)

	for resps := range mmcli.Run(cmd) {
		for _, resp := range resps.Resp {
			if resp.Error != "" {
				continue
			}

			for _, m := range re.FindAllStringSubmatch(resp.Response, -1) {
				queued += len(strings.Split(m[1], ","))
			}
		}
	}

	// `ns queue` will be empty once queued VMs have been launched.

	if queued == 0 {
		cmd.Command = "vm info"
		cmd.Columns = []string{"state"}

		status := mmcli.RunTabular(cmd)

		if len(status) == 0 {
			return 0.0, nil
		}

		for _, s := range status {
			if s["state"] == "BUILDING" {
				queued++
			}
		}
	}

	return float64(queued) / float64(expected), nil

}

func (this Minimega) GetVMInfo(opts ...Option) VMs {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vm info"
	cmd.Columns = []string{"host", "name", "state", "uptime", "vlan", "tap", "memory", "vcpus", "disks"}

	if o.vm != "" {
		cmd.Filters = []string{"name=" + o.vm}
	}

	var vms VMs

	for _, row := range mmcli.RunTabular(cmd) {
		var vm VM

		vm.Host = row["host"]
		vm.Name = row["name"]

		vm.Running = row["state"] == "RUNNING"
		//vm.State = row["state"]

		s := row["vlan"]
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		s = strings.TrimSpace(s)

		if s != "" {
			vm.Networks = strings.Split(s, ", ")
		}

		s = row["tap"]
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		s = strings.TrimSpace(s)

		if s != "" {
			vm.Taps = strings.Split(s, ", ")
		}

		//Make sure the vm name is set prior to
		//calling "GetVMCaptures" as the vm name is not always
		//set when calling GetVMInfo
		vm.Captures = this.GetVMCaptures(NS(o.ns),VMName(vm.Name))
		
		uptime, err := time.ParseDuration(row["uptime"])
		if err == nil {
			vm.Uptime = uptime.Seconds()
		}

		vm.RAM, _ = strconv.Atoi(row["memory"])
		vm.CPUs, _ = strconv.Atoi(row["vcpus"])

		// TODO: confirm multiple disks are separated by whitespace.
		disk := strings.Fields(row["disks"])[0]
		// diskspec can include multiple settings separated by comma. Path to disk
		// will always be first setting.
		disk = strings.Split(disk, ",")[0]

		cmd = mmcli.NewCommand()
		cmd.Command = "disk info " + disk

		// Only expect one row returned
		resp := mmcli.RunTabular(cmd)[0]

		if resp["backingfile"] == "" {
			vm.Disk = resp["image"]
		} else {
			vm.Disk = resp["backingfile"]
		}

		vms = append(vms, vm)
	}

	return vms
}

func (Minimega) GetVMScreenshot(opts ...Option) ([]byte, error) {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm screenshot %s file /dev/null %s", o.vm, o.screenshotSize)

	var screenshot []byte

	for resps := range mmcli.Run(cmd) {
		for _, resp := range resps.Resp {
			if resp.Error != "" {
				if strings.HasPrefix(resp.Error, "vm not running:") {
					continue
				} else if resp.Error == "cannot take screenshot of container" {
					continue
				}

				// Unknown error
				return nil, fmt.Errorf("unknown error getting VM screenshot: %s", resp.Error)
			}

			if resp.Data == nil {
				return nil, fmt.Errorf("not found")
			}

			if screenshot == nil {
				var err error

				screenshot, err = base64.StdEncoding.DecodeString(resp.Data.(string))
				if err != nil {
					return nil, fmt.Errorf("decoding screenshot: %s", err)
				}
			} else {
				return nil, fmt.Errorf("received more than one screenshot")
			}
		}
	}

	if screenshot == nil {
		return nil, fmt.Errorf("not found")
	}

	return screenshot, nil
}

func (Minimega) GetVNCEndpoint(opts ...Option) (string, error) {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vm info"
	cmd.Columns = []string{"host", "vnc_port"}
	cmd.Filters = []string{"type=kvm", fmt.Sprintf("name=%s", o.vm)}

	var endpoint string

	for _, vm := range mmcli.RunTabular(cmd) {
		endpoint = fmt.Sprintf("%s:%s", vm["host"], vm["vnc_port"])
	}

	if endpoint == "" {
		return "", fmt.Errorf("not found")
	}

	return endpoint, nil
}

func (Minimega) StartVM(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm start %s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("starting VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) StopVM(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm stop %s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("stopping VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) RedeployVM(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)

	cmd.Command = "vm config clone " + o.vm
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("cloning VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	cmd.Command = "clear vm config migrate"
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("clearing config for VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	cmd.Command = "vm kill " + o.vm
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("killing VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	if err := flush(o.ns); err != nil {
		return err
	}

	if o.cpu != 0 {
		cmd.Command = fmt.Sprintf("vm config vcpus %d", o.cpu)

		if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
			return fmt.Errorf("configuring VCPUs for VM %s in namespace %s: %w", o.vm, o.ns, err)
		}
	}

	if o.mem != 0 {
		cmd.Command = fmt.Sprintf("vm config mem %d", o.mem)

		if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
			return fmt.Errorf("configuring memory for VM %s in namespace %s: %w", o.vm, o.ns, err)
		}
	}

	if o.disk != "" {
		var disk string

		if len(o.injects) == 0 {
			disk = o.disk
		} else {
			cmd.Command = "vm config disk"
			cmd.Columns = []string{"disks"}
			cmd.Filters = []string{"name=" + o.vm}

			config := mmcli.RunTabular(cmd)

			cmd.Columns = nil
			cmd.Filters = nil

			if len(config) == 0 {
				return fmt.Errorf("disk config not found for VM %s in namespace %s", o.vm, o.ns)
			}

			// Should only be one row of data since we filter by VM name above.
			status := config[0]

			disk = filepath.Base(status["disks"])

			if strings.Contains(disk, "_snapshot") {
				cmd.Command = fmt.Sprintf("disk snapshot %s %s", o.disk, disk)

				if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
					return fmt.Errorf("snapshotting disk for VM %s in namespace %s: %w", o.vm, o.ns, err)
				}

				if err := inject(disk, o.injectPart, o.injects...); err != nil {
					return err
				}
			} else {
				disk = o.disk
			}
		}

		cmd.Command = "vm config disk " + disk

		if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
			return fmt.Errorf("configuring disk for VM %s in namespace %s: %w", o.vm, o.ns, err)
		}
	}

	cmd.Command = "vm launch kvm " + o.vm
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("scheduling VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	cmd.Command = "vm launch"
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("launching scheduled VMs in namespace %s: %w", o.ns, err)
	}

	cmd.Command = fmt.Sprintf("vm start %s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("starting VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) KillVM(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm kill %s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("killing VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	return flush(o.ns)
}

func (Minimega) GetVMHost(opts ...Option) (string, error) {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vm info"
	cmd.Columns = []string{"host"}
	cmd.Filters = []string{"name=" + o.vm}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return "", fmt.Errorf("VM %s not found", o.vm)
	}

	return status[0]["host"], nil
}

func (Minimega) GetVMState(opts ...Option) (string, error) {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vm info"
	cmd.Columns = []string{"state"}
	cmd.Filters = []string{"name=" + o.vm}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return "", fmt.Errorf("VM %s not found", o.vm)
	}

	return status[0]["state"], nil
}



func (Minimega) ConnectVMInterface(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm net connect %s %d %s", o.vm, o.connectIface, o.connectVLAN)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("connecting interface %d on VM %s to VLAN %s in namespace %s: %w", o.connectIface, o.vm, o.connectVLAN, o.ns, err)
	}

	return nil
}

func (Minimega) DisconnectVMInterface(opts ...Option) error {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("vm net disconnect %s %d", o.vm, o.connectIface)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("disconnecting interface %d on VM %s in namespace %s: %w", o.connectIface, o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) StartVMCapture(opts ...Option) error {
	o := NewOptions(opts...)

	captures := GetVMCaptures(opts...)

	for _, capture := range captures {
		if capture.Interface == o.captureIface {
			return ErrCaptureExists
		}
	}

	if filepath.IsAbs(o.captureFile) {
		return fmt.Errorf("path for capture file should not be absolute")
	}

	host, err := GetVMHost(opts...)
	if err != nil {
		return fmt.Errorf("unable to determine what host the VM is scheduled on: %w", err)
	}

	var cmdPrefix string

	if !IsHeadnode(host) {
		cmdPrefix = "mesh send " + host
	}

	dir := common.PhenixBase + "/images/" + filepath.Dir(o.captureFile)
	cmd := mmcli.NewCommand()
	cmd.Command = fmt.Sprintf("%s shell mkdir -p %s", cmdPrefix, dir)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("ensuring experiment files directory exists: %w", err)
	}

	cmd = mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("capture pcap vm %s %d %s", o.vm, o.captureIface, o.captureFile)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("starting VM capture for interface %d on VM %s in namespace %s: %w", o.captureIface, o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) StopVMCapture(opts ...Option) error {
	captures := GetVMCaptures(opts...)

	if len(captures) == 0 {
		return ErrNoCaptures
	}

	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("capture pcap delete vm %s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("deleting VM captures for VM %s in namespace %s: %w", o.vm, o.ns, err)
	}

	return nil
}

func (Minimega) GetExperimentCaptures(opts ...Option) []Capture {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "capture"
	cmd.Columns = []string{"interface", "path"}

	var captures []Capture

	for _, row := range mmcli.RunTabular(cmd) {
		// `interface` column will be in the form of <vm_name>:<iface_idx>
		iface := strings.Split(row["interface"], ":")

		vm := iface[0]
		idx, _ := strconv.Atoi(iface[1])

		capture := Capture{
			VM:        vm,
			Interface: idx,
			Filepath:  row["path"],
		}

		captures = append(captures, capture)
	}

	return captures
}

func (this Minimega) GetVMCaptures(opts ...Option) []Capture {
	o := NewOptions(opts...)

	var (
		captures = this.GetExperimentCaptures(opts...)
		keep     []Capture
	)

	for _, capture := range captures {
		if capture.VM == o.vm {
			keep = append(keep, capture)
		}
	}

	return keep
}

func (Minimega) GetClusterHosts(schedOnly bool) (Hosts, error) {
	// Get headnode details
	hosts, err := processNamespaceHosts("minimega")
	if err != nil {
		return nil, fmt.Errorf("processing headnode details: %w", err)
	}

	if len(hosts) == 0 {
		return []Host{}, fmt.Errorf("no cluster hosts found")
	}

	head := hosts[0]
	head.Schedulable = false
	head.Headnode = true

	var cluster []Host

	// Clear dummy namespace used for getting compute nodes in case a new compute
	// node has been added since the last time the dummy namespace was created.
	ClearNamespace("__phenix__")

	// Get compute nodes details
	hosts, err = processNamespaceHosts("__phenix__")
	if err != nil {
		return nil, fmt.Errorf("processing compute nodes details: %w", err)
	}

	for _, host := range hosts {
		// This will happen if the headnode is included as a compute node
		// (ie. when there's only one node in the cluster).
		if host.Name == head.Name {
			head.Schedulable = true
			continue
		}

		host.Name = common.TrimHostnameSuffixes(host.Name)
		host.Schedulable = true

		cluster = append(cluster, host)
	}

	if schedOnly && !head.Schedulable {
		return cluster, nil
	}

	head.Name = common.TrimHostnameSuffixes(head.Name)

	cluster = append(cluster, head)

	return cluster, nil
}

func (Minimega) Headnode() string {
	// Get headnode details
	hosts, _ := processNamespaceHosts("minimega")

	if len(hosts) == 0 {
		return "" // ???
	}

	headnode := hosts[0].Name

	// Trim host name suffixes (like -minimega, or -phenix) potentially added to
	// Docker containers by Docker Compose config.
	return common.TrimHostnameSuffixes(headnode)
}

func (this Minimega) IsHeadnode(node string) bool {
	// Trim node name suffixes (like -minimega, or -phenix) potentially added to
	// Docker containers by Docker Compose config.
	node = common.TrimHostnameSuffixes(node)

	return node == this.Headnode()
}

func (Minimega) GetVLANs(opts ...Option) (map[string]int, error) {
	o := NewOptions(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vlans"

	var (
		vlans  = make(map[string]int)
		status = mmcli.RunTabular(cmd)
	)

	for _, row := range status {
		alias := row["alias"]
		id, err := strconv.Atoi(row["vlan"])
		if err != nil {
			return nil, fmt.Errorf("converting VLAN ID to integer: %w", err)
		}

		vlans[alias] = id
	}

	return vlans, nil
}

func (Minimega) IsC2ClientActive(opts ...C2Option) error {
	o := NewC2Options(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "vm info"
	cmd.Columns = []string{"cc_active"}
	cmd.Filters = []string{"name=" + o.vm}

	rows := mmcli.RunTabular(cmd)

	if len(rows) == 0 {
		return fmt.Errorf("no VMs returned for host %s", o.vm)
	}

	if rows[0]["cc_active"] != "true" {
		return ErrC2ClientNotActive
	}

	return nil
}

func (this Minimega) ExecC2Command(opts ...C2Option) (string, error) {
	ccMu.Lock()
	defer ccMu.Unlock()

	if err := this.IsC2ClientActive(opts...); err != nil {
		return "", fmt.Errorf("cannot execute command: %w", err)
	}

	o := NewC2Options(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = fmt.Sprintf("cc filter name=%s", o.vm)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return "", fmt.Errorf("setting host filter to %s: %w", o.vm, err)
	}

	cmd.Command = fmt.Sprintf("cc exec %s", o.command)

	data, err := mmcli.SingleDataResponse(mmcli.Run(cmd))
	if err != nil {
		return "", fmt.Errorf("executing command %s: %w", o.command, err)
	}

	// This will the the ID for the cc exec command
	return fmt.Sprintf("%v", data), nil
}

func (Minimega) WaitForC2Response(ctx context.Context, opts ...C2Option) (string, error) {
	o := NewC2Options(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "cc commands"
	cmd.Columns = []string{"id", "responses"}
	cmd.Filters = []string{"id=" + o.commandID}

	// Multiple rows will come back for each command ID, one row per cluster host.
	// Because the `ExecC2Command` sets the filter to a specific VM, only one of
	// the rows will have a response since a VM can only run on a single cluster
	// host.

	err := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(o.timeout):
				return fmt.Errorf("timeout waiting for response for command %s", o.commandID)
			default:
				rows := mmcli.RunTabular(cmd)

				if len(rows) == 0 {
					return fmt.Errorf("no commands returned for ID %s", o.commandID)
				}

				if rid := rows[0]["id"]; rid != o.commandID {
					return fmt.Errorf("wrong command returned: %s", rid)
				}

				for _, row := range rows {
					if row["responses"] != "0" {
						return nil
					}
				}

				time.Sleep(1 * time.Second)
			}
		}
	}()

	if err != nil {
		return "", err
	}

	cmd.Command = fmt.Sprintf("cc response %s raw", o.commandID)

	resp, err := mmcli.SingleResponse(mmcli.Run(cmd))
	if err != nil {
		return "", fmt.Errorf("getting response for command %s: %w", o.commandID, err)
	}

	return resp, nil
}

func (Minimega) ClearC2Responses(opts ...C2Option) error {
	o := NewC2Options(opts...)

	cmd := mmcli.NewNamespacedCommand(o.ns)
	cmd.Command = "clear cc responses"

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("clearing C2 responses for namespace %s: %w", o.ns, err)
	}

	return nil
}

func flush(ns string) error {
	cmd := mmcli.NewNamespacedCommand(ns)
	cmd.Command = "vm flush"

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("flushing VMs in namespace %s: %w", ns, err)
	}

	return nil
}

func inject(disk string, part int, injects ...string) error {
	files := strings.Join(injects, " ")

	cmd := mmcli.NewCommand()
	cmd.Command = fmt.Sprintf("disk inject %s:%d files %s", disk, part, files)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("injecting files into disk %s: %w", disk, err)
	}

	return nil
}

func processNamespaceHosts(namespace string) (Hosts, error) {
	cmd := mmcli.NewNamespacedCommand(namespace)
	cmd.Command = "host"

	var (
		hosts  Hosts
		status = mmcli.RunTabular(cmd)
	)

	for _, row := range status {
		host := Host{Name: row["host"]}
		host.CPUs, _ = strconv.Atoi(row["cpus"])
		host.CPUCommit, _ = strconv.Atoi(row["cpucommit"])
		host.Load = strings.Split(row["load"], " ")
		host.MemUsed, _ = strconv.Atoi(row["memused"])
		host.MemTotal, _ = strconv.Atoi(row["memtotal"])
		host.MemCommit, _ = strconv.Atoi(row["memcommit"])
		host.VMs, _ = strconv.Atoi(row["vms"])

		host.Tx, _ = strconv.ParseFloat(row["tx"], 64)
		host.Rx, _ = strconv.ParseFloat(row["rx"], 64)
		host.Bandwidth = fmt.Sprintf("rx: %.1f / tx: %.1f", host.Rx, host.Tx)
		host.NetCommit, _ = strconv.Atoi(row["netcommit"])

		uptime, _ := time.ParseDuration(row["uptime"])
		host.Uptime = uptime.Seconds()

		hosts = append(hosts, host)
	}

	return hosts, nil
}
