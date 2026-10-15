//go:build windows

package appfinder

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/eaopen/cloudfile-local-agent/internal/winreg"
)

func platformApplications() []Application {
	roots := programRoots()
	applications := make([]Application, 0, 12)
	appendFirst := func(id, name string, extensions, patterns []string) {
		for _, executable := range appPathNames[id] {
			if path := appPathExecutable(executable); path != "" {
				applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{path, "{file}"}})
				return
			}
		}
		for _, pattern := range patterns {
			for _, path := range expandPatterns(roots, pattern) {
				if isExecutable(path) {
					applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{path, "{file}"}})
					return
				}
			}
		}
	}

	appendFirst("word", "Microsoft Word", []string{"doc", "docx", "dot", "dotx", "rtf"}, []string{
		"Microsoft Office/root/Office*/WINWORD.EXE", "Microsoft Office/Office*/WINWORD.EXE"})
	appendFirst("excel", "Microsoft Excel", []string{"xls", "xlsx", "xlsm", "xlsb", "csv"}, []string{
		"Microsoft Office/root/Office*/EXCEL.EXE", "Microsoft Office/Office*/EXCEL.EXE"})
	appendFirst("powerpoint", "Microsoft PowerPoint", []string{"ppt", "pptx", "pptm", "ppsx"}, []string{
		"Microsoft Office/root/Office*/POWERPNT.EXE", "Microsoft Office/Office*/POWERPNT.EXE"})
	appendFirst("visio", "Microsoft Visio", []string{"vsd", "vsdx", "vssx"}, []string{
		"Microsoft Office/root/Office*/VISIO.EXE", "Microsoft Office/Office*/VISIO.EXE"})
	appendFirst("libreoffice", "LibreOffice", []string{"doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "csv", "rtf"}, []string{
		"LibreOffice/program/soffice.exe"})

	appendFirst("autocad", "AutoCAD", []string{"dwg", "dxf", "dwt"}, []string{"Autodesk/AutoCAD*/acad.exe"})
	appendFirst("bricscad", "BricsCAD", []string{"dwg", "dxf", "dwt"}, []string{"Bricsys/BricsCAD*/bricscad.exe"})
	appendFirst("draftsight", "DraftSight", []string{"dwg", "dxf", "dwt"}, []string{"Dassault Systemes/DraftSight*/bin/DraftSight.exe"})
	appendFirst("revit", "Autodesk Revit", []string{"rvt", "rfa", "rte", "rft"}, []string{"Autodesk/Revit*/Revit.exe"})
	appendFirst("solidworks", "SOLIDWORKS", []string{"sldprt", "sldasm", "slddrw"}, []string{"SOLIDWORKS Corp/SOLIDWORKS/SLDWORKS.exe"})
	appendFirst("creo", "PTC Creo", []string{"prt", "asm", "drw"}, []string{"PTC/Creo*/Common Files/bin/parametric.exe"})
	appendFirst("nx", "Siemens NX", []string{"prt", "asm"}, []string{"Siemens/NX*/NXBIN/ugraf.exe"})
	appendFirst("catia", "CATIA", []string{"catpart", "catproduct", "catdrawing"}, []string{"Dassault Systemes/B*/win_b64/code/bin/CNEXT.exe"})
	appendFirst("sketchup", "SketchUp", []string{"skp"}, []string{"SketchUp/SketchUp*/SketchUp.exe"})
	appendFirst("rhino", "Rhino", []string{"3dm"}, []string{"Rhino*/System/Rhino.exe"})
	appendFirst("freecad", "FreeCAD", []string{"step", "stp", "iges", "igs", "stl", "obj", "fcstd"}, []string{"FreeCAD*/bin/FreeCAD.exe"})
	return applications
}

var appPathNames = map[string][]string{
	"word":        {"WINWORD.EXE"},
	"excel":       {"EXCEL.EXE"},
	"powerpoint":  {"POWERPNT.EXE"},
	"visio":       {"VISIO.EXE"},
	"libreoffice": {"soffice.exe"},
	"autocad":     {"acad.exe"},
	"bricscad":    {"bricscad.exe"},
	"draftsight":  {"DraftSight.exe"},
	"revit":       {"Revit.exe"},
	"solidworks":  {"SLDWORKS.exe"},
	"creo":        {"parametric.exe"},
	"nx":          {"ugraf.exe"},
	"catia":       {"CNEXT.exe"},
	"sketchup":    {"SketchUp.exe"},
	"rhino":       {"Rhino.exe"},
	"freecad":     {"FreeCAD.exe"},
}

// App Paths is the documented Windows registration point for mapping an
// executable name to its absolute path.  It catches per-user installs that
// do not live under Program Files; a failed registry query merely falls back
// to the conservative known-directory scan below.
//
// 这里刻意**不启动 reg.exe**：它是控制台程序，宿主（Chrome 起的 native host）
// 没有控制台，于是每查一次就弹一个黑框一闪而过；16 个应用 × 3 个键 = 48 次，
// 用户看到的就是「啪啪啪一堆黑窗口」。改成进程内直接读注册表：没有窗口，
// 单次状态查询从 0.7~1.6 秒降到毫秒级。
func appPathExecutable(executable string) string {
	subkey := `Software\Microsoft\Windows\CurrentVersion\App Paths\` + executable
	targets := []struct {
		hive   winreg.Hive
		subkey string
	}{
		{winreg.CurrentUser, subkey},
		{winreg.LocalMachine, subkey},
		// 32 位应用在 64 位系统上注册到这里。
		{winreg.LocalMachine, `Software\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\` + executable},
	}
	for _, target := range targets {
		// 空名字表示该键的默认值，正是 App Paths 存 exe 路径的地方。
		path, err := winreg.ReadString(target.hive, target.subkey, "")
		if err != nil {
			continue
		}
		path = strings.TrimSpace(path)
		if isExecutable(path) {
			return path
		}
	}
	return ""
}

func programRoots() []string {
	values := []string{os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
	roots := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			roots = append(roots, value)
		}
	}
	return roots
}

func expandPatterns(roots []string, pattern string) []string {
	paths := make([]string, 0)
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		paths = append(paths, matches...)
	}
	return paths
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
