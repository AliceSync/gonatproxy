package service

// RunCLI implements `deepseek-<role> service <sub>`:
//
//	service install             write unit + daemon-reload + enable (separate)
//	service enable              autostart only (does NOT start now)
//	service start               start now
//	service stop|restart|status
//	service show                print the unit that would be installed
//
// It is shared by the client and natclient binaries (no GUI).
import (
	"fmt"
	"os"
	"path/filepath"
)

// RunCLI handles the `service` subcommand for a role. Returns process exit code.
func RunCLI(role, cfgPath string, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Printf("usage: deepseek-%s service <sub>\n\n", role)
		fmt.Print(`  install   generate deepseek-<role>.service + enable (separate from start)
  enable    enable autostart (does not start now)
  start     start now
  stop|restart|status
  show      print the unit that would be installed
`)
		return 0
	}
	unit := RoleUnitName(role)
	switch args[0] {
	case "install":
		path, err := InstallUnit(role, cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "service install:", err)
			return 1
		}
		fmt.Printf("unit written: %s\n", path)
		if out, err := Sysctl("enable", unit); err != nil {
			fmt.Fprintf(os.Stderr, "warning: enable 失败（%s）。请确认单元目录属于 systemd 或稍后用 web 控制台启用。\n", out)
		} else if out != "" {
			fmt.Println(out)
		}
		fmt.Printf("已启用开机自启（enable，未启动）. 现在启动请运行: deepseek-%s service start   或   systemctl start %s\n", role, unit)
		return 0
	case "show":
		exe, _ := os.Executable()
		u, g := CurrentUser()
		b, err := RenderUnit(role, exe, cfgPath, u, g)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		os.Stdout.Write(b)
		return 0
	case "enable":
		return sysctlCLI(unit, "enable")
	case "start":
		if !SystemdActive() {
			fmt.Fprintln(os.Stderr, "systemd 未运行。请直接用前台运行 deepseek-"+role+" -config <cfg>。")
			return 1
		}
		return sysctlCLILive(role, unit, "start", "启动前请先停止正在前台运行的旧实例（Ctrl-C），否则端口可能冲突。")
	case "stop", "restart":
		return sysctlCLILive(role, unit, args[0], "该操作作用于 systemd 管理的服务实例。")
	case "status":
		return sysctlCLI(unit, "status")
	default:
		fmt.Fprintln(os.Stderr, "unknown service subcommand:", args[0])
		return 2
	}
}

func sysctlCLI(unit, action string) int {
	out, err := Sysctl(action, unit)
	if err != nil {
		fmt.Fprintln(os.Stderr, action+":", out)
		return 1
	}
	if out != "" {
		fmt.Println(out)
	}
	return 0
}

func sysctlCLILive(role, unit, action, hint string) int {
	out, err := Sysctl(action, unit)
	if err != nil {
		fmt.Fprintln(os.Stderr, action+" 失败:", out)
		fmt.Fprintln(os.Stderr, "提示:", hint)
		return 1
	}
	if out != "" {
		fmt.Println(out)
	}
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".unit-*")
	if err != nil {
		return err
	}
	tmppath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmppath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmppath)
		return err
	}
	os.Chmod(tmppath, 0o644)
	if err := os.Rename(tmppath, path); err != nil {
		os.Remove(tmppath)
		return err
	}
	return nil
}
