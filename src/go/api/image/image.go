package image

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"phenix/internal/mm/mmcli"
	"phenix/store"
	"phenix/tmpl"
	"phenix/types"
	v1 "phenix/types/version/v1"
	"phenix/util/shell"

	"github.com/activeshadow/structs"
	"github.com/mitchellh/mapstructure"
)

const (
	V_VERBOSE   int = 1
	V_VVERBOSE  int = 2
	V_VVVERBOSE int = 4
)

var (
	ErrMinicccNotFound   = fmt.Errorf("miniccc executable not found")
	ErrProtonukeNotFound = fmt.Errorf("protonuke executable not found")
)

// SetDefaults will set default settings to image values if none are set by the
// user. The default values are:
//   -- Image size at `5G`
//   -- The variant is `minbase`
//   -- The release is `bionic` (Ubuntu 18.04.4 LTS)
//   -- The mirror is `http://us.archive.ubuntu.com/ubuntu/`
//   -- The image format is `raw`
// This will also remove empty strings in packages and overlays; if overlays are
// used, the default `/phenix/images` directory is added to the overlay name.
// Based on the variant value, specific constants will be included during the
// create sub-command. The values are passed from the `constants.go` file. An
// error will be returned if the variant value is not valid (acceptable values
// are `minbase`, `mingui`, `kali`, or `brash`).
func SetDefaults(img *v1.Image) error {
	if img.Size == "" {
		img.Size = "5G"
	}

	if img.Variant == "" {
		img.Variant = "minbase"
	}

	if img.Release == "" {
		img.Release = "bionic"
	}

	if img.Mirror == "" {
		img.Mirror = "http://us.archive.ubuntu.com/ubuntu/"
	}

	if img.Format == "" {
		img.Format = "raw"
	}

	if !strings.Contains(img.DebAppend, "--components=") {
		if img.Release == "kali" || img.Release == "kali-rolling" {
			img.DebAppend += " --components=" + strings.Join(PACKAGES_COMPONENTS_KALI, ",")
		} else {
			img.DebAppend += " --components=" + strings.Join(PACKAGES_COMPONENTS, ",")
		}
	}

	switch img.Variant {
	case "minbase":
		if img.Release == "kali" || img.Release == "kali-rolling" {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_KALI...)
		} else {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_BIONIC...)
		}
	case "mingui":
		if img.Release == "kali" || img.Release == "kali-rolling" {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_KALI...)
			img.Packages = append(img.Packages, PACKAGES_MINGUI...)
			img.Packages = append(img.Packages, PACKAGES_MINGUI_KALI...)
		} else {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_BIONIC...)
			img.Packages = append(img.Packages, PACKAGES_MINGUI...)
			if img.Release == "xenial" {
				img.Packages = append(img.Packages, "qupzilla")
			} else {
				img.Packages = append(img.Packages, "falkon")
			}
		}
	case "brash":
		if img.Release == "kali" || img.Release == "kali-rolling" {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_KALI...)
			img.Packages = append(img.Packages, PACKAGES_BRASH...)
		} else {
			img.Packages = append(img.Packages, PACKAGES_DEFAULT...)
			img.Packages = append(img.Packages, PACKAGES_BIONIC...)
			img.Packages = append(img.Packages, PACKAGES_BRASH...)
		}
	default:
		return fmt.Errorf("variant %s is not implemented", img.Variant)
	}

	img.Scripts = make(map[string]string)

	addScriptToImage(img, "POSTBUILD_APT_CLEANUP", POSTBUILD_APT_CLEANUP)

	switch img.Variant {
	case "minbase", "mingui":
		addScriptToImage(img, "POSTBUILD_NO_ROOT_PASSWD", POSTBUILD_NO_ROOT_PASSWD)
		addScriptToImage(img, "POSTBUILD_PHENIX_HOSTNAME", POSTBUILD_PHENIX_HOSTNAME)
	case "brash":
	default:
		return fmt.Errorf("variant %s is not implemented", img.Variant)
	}

	if len(img.ScriptPaths) > 0 {
		for _, p := range img.ScriptPaths {
			if err := addScriptToImage(img, p, ""); err != nil {
				return fmt.Errorf("adding script %s to image config: %w", p, err)
			}
		}
	}

	return nil
}

// Create collects image values from user input at command line, creates an
// image configuration, and then persists it to the store. SetDefaults is used
// to set default values if the user did not include any in the image create
// sub-command. This sub-command requires an image `name`. It will return any
// errors encoutered while creating the configuration.
func Create(name string, img *v1.Image) error {
	if name == "" {
		return fmt.Errorf("image name is required to create an image")
	}

	if err := SetDefaults(img); err != nil {
		return fmt.Errorf("setting image defaults: %w", err)
	}

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Image",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     structs.MapDefaultCase(img, structs.CASESNAKE),
	}

	if err := store.Create(&c); err != nil {
		return fmt.Errorf("storing image config: %w", err)
	}

	return nil
}

// CreateFromConfig will take in an existing image configuration by name and
// modify overlay, packages, and scripts as passed by the user. It will then
// persist a new image configuration to the store. Any errors enountered will be
// passed when creating a new image configuration, retrieving the exisitng image
// configuration file, or storing the new image configuration file in the store.
func CreateFromConfig(name, saveas string, overlays, packages, scripts []string) error {
	c, err := store.NewConfig("image/" + name)
	if err != nil {
		return fmt.Errorf("creating new image config for %s: %w", name, err)
	}

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting config from store: %w", err)
	}

	var img v1.Image

	if err := mapstructure.Decode(c.Spec, &img); err != nil {
		return fmt.Errorf("decoding image spec: %w", err)
	}

	if err := SetDefaults(&img); err != nil {
		return fmt.Errorf("setting image defaults: %w", err)
	}

	c.Metadata.Name = saveas

	if len(overlays) > 0 {
		img.Overlays = append(img.Overlays, overlays...)
	}

	if len(packages) > 0 {
		img.Packages = append(img.Packages, packages...)
	}

	if len(scripts) > 0 {
		for _, s := range scripts {
			if err := addScriptToImage(&img, s, ""); err != nil {
				return fmt.Errorf("adding script %s to image config: %w", s, err)
			}
		}
	}

	c.Spec = structs.MapDefaultCase(img, structs.CASESNAKE)

	if err := store.Create(c); err != nil {
		return fmt.Errorf("storing new image config %s in store: %w", saveas, err)
	}

	return nil
}

// Build uses the image configuration `name` passed by users to build an image.
// If verbosity is set, `vmdb` will output progress as it builds the image.
// Otherwise, there will only be output if an error is encountered. The image
// configuration is used with a template to build the `vmdb` configuration file
// and then pass it to the shelled out `vmdb` command. This expects the `vmdb`
// application is in the `$PATH`. Any errors encountered will be returned during
// the process of getting an existing image configuration, decoding it,
// generating the `vmdb` verbosconfiguration file, or executing the `vmdb` command.
func Build(ctx context.Context, name string, verbosity int, cache bool, dryrun bool, output string) error {
	c, _ := store.NewConfig("image/" + name)

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting image config %s from store: %w", name, err)
	}

	var img v1.Image

	if err := mapstructure.Decode(c.Spec, &img); err != nil {
		return fmt.Errorf("decoding image spec: %w", err)
	}

	if verbosity >= V_VVVERBOSE {
		img.VerboseLogs = true
	}

	img.Cache = cache

	// The Kali package repos use `kali-rolling` as the release name.
	if img.Release == "kali" {
		img.Release = "kali-rolling"
	}

	if img.IncludeMiniccc {
		img.Overlays = append(img.Overlays, "/usr/local/share/minimega/overlays/miniccc")
	}

	if img.IncludeProtonuke {
		img.Overlays = append(img.Overlays, "/usr/local/share/minimega/overlays/protonuke")
	}

	filename := output + "/" + name + ".vmdb"

	if err := tmpl.CreateFileFromTemplate("vmdb.tmpl", img, filename); err != nil {
		return fmt.Errorf("generate vmdb config from template: %w", err)
	}

	if !dryrun && !shell.CommandExists("vmdb2") {
		return fmt.Errorf("vmdb2 app does not exist in your path")
	}

	args := []string{
		filename,
		"--output", output + "/" + name,
		"--rootfs-tarball", output + "/" + name + ".tar",
	}

	if verbosity >= V_VERBOSE {
		args = append(args, "-v")
	}

	if verbosity >= V_VVERBOSE {
		args = append(args, "--log", "stderr")
	}

	if dryrun {
		fmt.Printf("DRY RUN: vmdb2 %s\n", strings.Join(args, " "))
	} else {
		cmd := exec.Command("vmdb2", args...)

		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting vmdb2 command: %w", err)
		}

		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				fmt.Println(scanner.Text())
			}
		}()

		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				fmt.Println(scanner.Text())
			}
		}()

		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("building image with vmdb2: %w", err)
		}
	}

	return nil
}

// List collects image configurations from the store. It returns a slice of all
// configurations. It will return any errors encountered while getting the list
// of image configurations.
func List() ([]types.Image, error) {
	configs, err := store.List("Image")
	if err != nil {
		return nil, fmt.Errorf("getting list of image configs from store: %w", err)
	}

	var images []types.Image

	for _, c := range configs {
		spec := new(v1.Image)

		if err := mapstructure.Decode(c.Spec, spec); err != nil {
			return nil, fmt.Errorf("decoding image spec: %w", err)
		}

		img := types.Image{Metadata: c.Metadata, Spec: spec}

		images = append(images, img)
	}

	return images, nil
}

// Update retrieves the named image configuration file from the store and will
// update scripts. First, it will verify the script is present on disk. If so,
// it will remove the existing script from the configuration file and update the
// file with updated. It will return any errors encountered during the process
// of creating a new image configuration, decoding it, or updating it in the
// store.
func Update(name string) error {
	c, err := store.NewConfig("image/" + name)
	if err != nil {
		return fmt.Errorf("creating new image config for %s: %w", name, err)
	}

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting config from store: %w", err)
	}

	var img v1.Image

	if err := mapstructure.Decode(c.Spec, &img); err != nil {
		return fmt.Errorf("decoding image spec: %w", err)
	}

	scripts := img.Scripts

	if len(scripts) > 0 {
		for k := range scripts {
			if _, err := os.Stat(k); err == nil {
				delete(img.Scripts, k)

				if err := addScriptToImage(&img, k, ""); err != nil {
					return fmt.Errorf("adding script %s to image config: %w", k, err)
				}
			}
		}
	}

	c.Spec = structs.MapDefaultCase(img, structs.CASESNAKE)

	if err := store.Update(c); err != nil {
		return fmt.Errorf("updating image config in store: %w", err)
	}

	return nil
}

// Append retrieves the named image configuration file from the store and will
// update it with overlays, packages, and scripts as passed by the user. It will
// return any errors encountered during the process of creating a new image
// configuration, decoding it, or updating it in the store.
func Append(name string, overlays, packages, scripts []string) error {
	c, err := store.NewConfig("image/" + name)
	if err != nil {
		return fmt.Errorf("creating new image config for %s: %w", name, err)
	}

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting config from store: %w", err)
	}

	var img v1.Image

	if err := mapstructure.Decode(c.Spec, &img); err != nil {
		return fmt.Errorf("decoding image spec: %w", err)
	}

	if len(overlays) > 0 {
		img.Overlays = append(img.Overlays, overlays...)
	}

	if len(packages) > 0 {
		img.Packages = append(img.Packages, packages...)
	}

	if len(scripts) > 0 {
		for _, s := range scripts {
			if err := addScriptToImage(&img, s, ""); err != nil {
				return fmt.Errorf("adding script %s to image config: %w", s, err)
			}
		}
	}

	c.Spec = structs.MapDefaultCase(img, structs.CASESNAKE)

	if err := store.Update(c); err != nil {
		return fmt.Errorf("updating image config in store: %w", err)
	}

	return nil
}

// Remove will update an existing image configuration by removing the overlays,
// packages, and scripts as passed by the user. It will return any errors
// encountered during the process of creating a new image configuration,
// decoding it, or updating it in the store.
func Remove(name string, overlays, packages, scripts []string) error {
	c, err := store.NewConfig("image/" + name)
	if err != nil {
		return fmt.Errorf("creating new image config for %s: %w", name, err)
	}

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting config from store: %w", err)
	}

	var img v1.Image

	if err := mapstructure.Decode(c.Spec, &img); err != nil {
		return fmt.Errorf("decoding image spec: %w", err)
	}

	if len(overlays) > 0 {
		o := img.Overlays[:0]

		for _, overlay := range img.Overlays {
			var match bool

			for _, n := range overlays {
				if n == overlay {
					match = true
					break
				}
			}

			if !match {
				o = append(o, overlay)
			}
		}

		img.Overlays = o
	}

	if len(packages) > 0 {
		p := img.Packages[:0]

		for _, pkg := range img.Packages {
			var match bool

			for _, n := range packages {
				if n == pkg {
					match = true
					break
				}
			}

			if !match {
				p = append(p, pkg)
			}
		}

		img.Packages = p
	}

	if len(scripts) > 0 {
		for _, s := range scripts {
			delete(img.Scripts, s)
		}
	}

	c.Spec = structs.MapDefaultCase(img, structs.CASESNAKE)

	if err := store.Update(c); err != nil {
		return fmt.Errorf("updating image config in store: %w", err)
	}

	return nil
}

func InjectMiniccc(agent, disk, svc string) error {
	// Assume partition 1 if no partition is specified.
	if parts := strings.Split(disk, ":"); len(parts) == 1 {
		disk = disk + ":1"
	}

	tmp := os.TempDir() + "/phenix"

	if err := os.MkdirAll(tmp, 0755); err != nil {
		return fmt.Errorf("creating temp phenix base directory: %w", err)
	}

	tmp, err := ioutil.TempDir(tmp, "")
	if err != nil {
		return fmt.Errorf("creating temp directory: %w", err)
	}

	defer os.RemoveAll(tmp)

	var injects []string

	if path.Ext(agent) == ".exe" { // assume Windows
		if err := tmpl.RestoreAsset(tmp, "miniccc/miniccc-scheduler.cmd"); err != nil {
			return fmt.Errorf("restoring miniccc scheduler for Windows: %w", err)
		}

		injects = []string{
			tmp + `/miniccc/miniccc-scheduler.cmd:"/ProgramData/Microsoft/Windows/Start Menu/Programs/Startup/miniccc-scheduler.cmd"`,
			agent + ":/minimega/miniccc.exe",
		}
	} else { // assume Linux
		if err := os.MkdirAll(tmp+"/miniccc/symlinks", 0755); err != nil {
			return fmt.Errorf("creating symlinks directory path: %w", err)
		}

		switch svc {
		case "systemd":
			if err := tmpl.RestoreAsset(tmp, "miniccc/miniccc.service"); err != nil {
				return fmt.Errorf("restoring miniccc systemd service for Linux: %w", err)
			}

			if err := os.Symlink("../miniccc.service", tmp+"/miniccc/symlinks/miniccc.service"); err != nil {
				return fmt.Errorf("generating systemd service link for Linux: %w", err)
			}

			injects = []string{
				tmp + "/miniccc/miniccc.service:/etc/systemd/system/miniccc.service",
				tmp + "/miniccc/symlinks/miniccc.service:/etc/systemd/system/multi-user.target.wants/miniccc.service",
				agent + ":/usr/local/bin/miniccc",
			}
		case "sysinitv":
			if err := tmpl.RestoreAsset(tmp, "miniccc/miniccc.init"); err != nil {
				return fmt.Errorf("restoring miniccc sysinitv service for Linux: %w", err)
			}

			os.Chmod(tmp+"/miniccc/miniccc.init", 0755)

			if err := os.Symlink("../init.d/miniccc", tmp+"/miniccc/symlinks/S99-miniccc"); err != nil {
				return fmt.Errorf("generating sysinitv service link for Linux: %w", err)
			}

			injects = []string{
				tmp + "/miniccc/miniccc.init:/etc/init.d/miniccc",
				tmp + "/miniccc/symlinks/S99-miniccc:/etc/rc5.d/S99-miniccc",
				agent + ":/usr/local/bin/miniccc",
			}
		default:
			return fmt.Errorf("unknown service %s specified", svc)
		}
	}

	if err := inject(disk, injects...); err != nil {
		return fmt.Errorf("injecting miniccc files into disk: %w", err)
	}

	return nil
}

func addScriptToImage(img *v1.Image, name, script string) error {
	if script == "" {
		u, err := url.Parse(name)
		if err != nil {
			return fmt.Errorf("parsing script path: %w", err)
		}

		// Default to file scheme if no scheme provided.
		if u.Scheme == "" {
			u.Scheme = "file"
		}

		var (
			loc  = u.Host + u.Path
			body io.ReadCloser
		)

		switch u.Scheme {
		case "http", "https":
			resp, err := http.Get(name)
			if err != nil {
				return fmt.Errorf("getting script via HTTP(s): %w", err)
			}

			body = resp.Body
		case "file":
			body, err = os.Open(loc)
			if err != nil {
				return fmt.Errorf("opening script file: %w", err)
			}
		default:
			return fmt.Errorf("scheme %s not supported for scripts", u.Scheme)
		}

		defer body.Close()

		contents, err := ioutil.ReadAll(body)
		if err != nil {
			return fmt.Errorf("processing script %s: %w", name, err)
		}

		script = string(contents)
	}

	img.Scripts[name] = script
	img.ScriptOrder = append(img.ScriptOrder, name)

	return nil
}

func inject(disk string, injects ...string) error {
	files := strings.Join(injects, " ")

	cmd := mmcli.NewCommand()
	cmd.Command = fmt.Sprintf("disk inject %s files %s", disk, files)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("injecting files into disk %s: %w", disk, err)
	}

	return nil
}
