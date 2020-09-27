// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fetch provides a way to fetch modules from a proxy.
package fetch

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/google/safehtml/template"
	"go.opencensus.io/trace"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/derrors"
	"golang.org/x/pkgsite/internal/fetch/dochtml"
	"golang.org/x/pkgsite/internal/fetch/internal/doc"
	"golang.org/x/pkgsite/internal/licenses"
	"golang.org/x/pkgsite/internal/log"
	"golang.org/x/pkgsite/internal/proxy"
	"golang.org/x/pkgsite/internal/source"
	"golang.org/x/pkgsite/internal/stdlib"
)

var (
	errModuleContainsNoPackages = errors.New("module contains 0 packages")
	errMalformedZip             = errors.New("module zip is malformed")
)

// ModuleInfo holds some basic, easy-to-get information about a module.
type ModuleInfo struct {
	ModulePath       string
	RequestedVersion string
	ResolvedVersion  string
	CommitTime       time.Time
	ZipSize          int64
	Error            error
}

// GetModuleInfo returns preliminary information about a module, from the
// proxy's .info and .zip endpoints (the latter via HEAD only, to avoid
// downloading the zip).
func GetModuleInfo(ctx context.Context, modulePath, requestedVersion string, proxyClient *proxy.Client) ModuleInfo {
	mi := ModuleInfo{
		ModulePath:       modulePath,
		RequestedVersion: requestedVersion,
	}
	defer derrors.Wrap(&mi.Error, "GetModuleInfo(%q, %q)", modulePath, requestedVersion)

	if modulePath == stdlib.ModulePath {
		resolvedVersion, zipSize, err := stdlib.ZipInfo(requestedVersion)
		if err != nil {
			mi.Error = err
			return mi
		}
		mi.ResolvedVersion = resolvedVersion
		mi.ZipSize = zipSize
		// CommitTime unknown
		return mi
	}
	info, err := proxyClient.GetInfo(ctx, modulePath, requestedVersion)
	if err != nil {
		mi.Error = err
		return mi
	}
	zipSize, err := proxyClient.GetZipSize(ctx, modulePath, info.Version)
	if err != nil {
		mi.Error = err
		return mi
	}
	mi.ResolvedVersion = info.Version
	mi.CommitTime = info.Time
	mi.ZipSize = zipSize
	return mi
}

type FetchResult struct {
	ModulePath           string
	RequestedVersion     string
	ResolvedVersion      string
	GoModPath            string
	Status               int
	Error                error
	Module               *internal.Module
	PackageVersionStates []*internal.PackageVersionState
}

// FetchModule queries the proxy or the Go repo using the ModuleInfo obtained
// from GetModuleInfo. It then downloads the module zip, and processes the
// contents to return an *internal.Module and related information.
//
// Even if err is non-nil, the result may contain useful information, like the go.mod path.
func FetchModule(ctx context.Context, mi ModuleInfo, proxyClient *proxy.Client, sourceClient *source.Client) (fr *FetchResult) {
	fr = &FetchResult{
		ModulePath:       mi.ModulePath,
		RequestedVersion: mi.RequestedVersion,
	}
	defer func() {
		if fr.Error != nil {
			derrors.Wrap(&fr.Error, "FetchModule(%q, %q)", mi.ModulePath, mi.RequestedVersion)
			fr.Status = derrors.ToStatus(fr.Error)
		}
		if fr.Status == 0 {
			fr.Status = http.StatusOK
		}
		log.Debugf(ctx, "memory after fetch of %s@%s: %dM", mi.ModulePath, mi.RequestedVersion, allocMeg())
	}()

	if mi.Error != nil {
		fr.Error = mi.Error
		return fr
	}

	var (
		commitTime time.Time
		zipReader  *zip.Reader
		err        error
	)
	fr.ResolvedVersion = mi.ResolvedVersion
	if mi.ModulePath == stdlib.ModulePath {
		zipReader, commitTime, err = stdlib.Zip(mi.ResolvedVersion)
		if err != nil {
			fr.Error = err
			return fr
		}
		fr.GoModPath = stdlib.ModulePath
	} else {
		commitTime = mi.CommitTime
		goModBytes, err := proxyClient.GetMod(ctx, mi.ModulePath, fr.ResolvedVersion)
		if err != nil {
			fr.Error = err
			return fr
		}
		goModPath := modfile.ModulePath(goModBytes)
		if goModPath == "" {
			fr.Error = fmt.Errorf("go.mod has no module path: %w", derrors.BadModule)
			return fr
		}
		fr.GoModPath = goModPath
		if goModPath != mi.ModulePath {
			// The module path in the go.mod file doesn't match the path of the
			// zip file. Don't insert the module. Store an AlternativeModule
			// status in module_version_states.
			fr.Error = fmt.Errorf("module path=%s, go.mod path=%s: %w", mi.ModulePath, goModPath, derrors.AlternativeModule)
			return fr
		}
		zipReader, err = proxyClient.GetZip(ctx, mi.ModulePath, fr.ResolvedVersion)
		if err != nil {
			fr.Error = err
			return fr
		}
	}
	mod, pvs, err := processZipFile(ctx, mi.ModulePath, fr.ResolvedVersion, commitTime, zipReader, sourceClient)
	if err != nil {
		fr.Error = err
		return fr
	}
	fr.Module = mod
	fr.PackageVersionStates = pvs
	if mi.ModulePath == stdlib.ModulePath {
		fr.Module.HasGoMod = true
	}
	for _, state := range fr.PackageVersionStates {
		if state.Status != http.StatusOK {
			fr.Status = derrors.ToStatus(derrors.HasIncompletePackages)
		}
	}
	return fr
}

// processZipFile extracts information from the module version zip.
func processZipFile(ctx context.Context, modulePath string, resolvedVersion string, commitTime time.Time, zipReader *zip.Reader, sourceClient *source.Client) (_ *internal.Module, _ []*internal.PackageVersionState, err error) {
	defer derrors.Wrap(&err, "processZipFile(%q, %q)", modulePath, resolvedVersion)

	ctx, span := trace.StartSpan(ctx, "fetch.processZipFile")
	defer span.End()

	sourceInfo, err := source.ModuleInfo(ctx, sourceClient, modulePath, resolvedVersion)
	if err != nil {
		log.Infof(ctx, "error getting source info: %v", err)
	}
	readmes, err := extractReadmesFromZip(modulePath, resolvedVersion, zipReader)
	if err != nil {
		return nil, nil, fmt.Errorf("extractReadmesFromZip(%q, %q, zipReader): %v", modulePath, resolvedVersion, err)
	}
	logf := func(format string, args ...interface{}) {
		log.Infof(ctx, format, args...)
	}
	d := licenses.NewDetector(modulePath, resolvedVersion, zipReader, logf)
	allLicenses := d.AllLicenses()
	packages, packageVersionStates, err := extractPackagesFromZip(ctx, modulePath, resolvedVersion, zipReader, d, sourceInfo)
	if errors.Is(err, errModuleContainsNoPackages) || errors.Is(err, errMalformedZip) {
		return nil, nil, fmt.Errorf("%v: %w", err.Error(), derrors.BadModule)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("extractPackagesFromZip(%q, %q, zipReader, %v): %v", modulePath, resolvedVersion, allLicenses, err)
	}
	hasGoMod := zipContainsFilename(zipReader, path.Join(moduleVersionDir(modulePath, resolvedVersion), "go.mod"))

	var readmeFilePath, readmeContents string
	for _, r := range readmes {
		if path.Dir(r.Filepath) != "." {
			continue
		}
		readmeFilePath = r.Filepath
		readmeContents = r.Contents
		break
	}
	return &internal.Module{
		LegacyModuleInfo: internal.LegacyModuleInfo{
			ModuleInfo: internal.ModuleInfo{
				ModulePath:        modulePath,
				Version:           resolvedVersion,
				CommitTime:        commitTime,
				IsRedistributable: d.ModuleIsRedistributable(),
				HasGoMod:          hasGoMod,
				SourceInfo:        sourceInfo,
			},
			LegacyReadmeFilePath: readmeFilePath,
			LegacyReadmeContents: readmeContents,
		},
		LegacyPackages: packages,
		Licenses:       allLicenses,
		Units:          moduleUnits(modulePath, resolvedVersion, packages, readmes, d),
	}, packageVersionStates, nil
}

// moduleVersionDir formats the content subdirectory for the given
// modulePath and version.
func moduleVersionDir(modulePath, version string) string {
	return fmt.Sprintf("%s@%s", modulePath, version)
}

// extractReadmesFromZip returns the file path and contents of all files from r
// that are README files.
func extractReadmesFromZip(modulePath, resolvedVersion string, r *zip.Reader) ([]*internal.Readme, error) {
	var readmes []*internal.Readme
	for _, zipFile := range r.File {
		if isReadme(zipFile.Name) {
			if zipFile.UncompressedSize64 > MaxFileSize {
				return nil, fmt.Errorf("file size %d exceeds max limit %d", zipFile.UncompressedSize64, MaxFileSize)
			}
			c, err := readZipFile(zipFile, MaxFileSize)
			if err != nil {
				return nil, err
			}
			readmes = append(readmes, &internal.Readme{
				Filepath: strings.TrimPrefix(zipFile.Name, moduleVersionDir(modulePath, resolvedVersion)+"/"),
				Contents: string(c),
			})

		}
	}
	return readmes, nil
}

var excludedReadmeExts = map[string]bool{".go": true, ".vendor": true}

// isReadme reports whether file is README or if the base name of file, with or
// without the extension, is equal to expectedFile. README.go files will return
// false. It is case insensitive. It operates on '/'-separated paths.
func isReadme(file string) bool {
	const expectedFile = "README"
	base := path.Base(file)
	ext := path.Ext(base)
	return !excludedReadmeExts[ext] && strings.EqualFold(strings.TrimSuffix(base, ext), expectedFile)
}

// extractPackagesFromZip returns a slice of packages from the module zip r.
// It matches against the given licenses to determine the subset of licenses
// that applies to each package.
// The second return value says whether any packages are "incomplete," meaning
// that they contained .go files but couldn't be processed due to current
// limitations of this site. The limitations are:
// * a maximum file size (MaxFileSize)
// * the particular set of build contexts we consider (goEnvs)
// * whether the import path is valid.
func extractPackagesFromZip(ctx context.Context, modulePath, resolvedVersion string, r *zip.Reader, d *licenses.Detector, sourceInfo *source.Info) (_ []*internal.LegacyPackage, _ []*internal.PackageVersionState, err error) {
	ctx, span := trace.StartSpan(ctx, "fetch.extractPackagesFromZip")
	defer span.End()
	defer func() {
		if e := recover(); e != nil {
			// The package processing code performs some sanity checks along the way.
			// None of the panics should occur, but if they do, we want to log them and
			// be able to find them. So, convert internal panics to internal errors here.
			err = fmt.Errorf("internal panic: %v\n\n%s", e, debug.Stack())
		}
	}()

	// The high-level approach is to split the processing of the zip file
	// into two phases:
	//
	// 	1. loop over all files, looking at file metadata only
	// 	2. process all files by reading their contents
	//
	// During phase 1, we populate the dirs map for each directory
	// that contains at least one .go file.

	var (
		// modulePrefix is the "<module>@<resolvedVersion>/" prefix that all files
		// are expected to have according to the zip archive layout specification
		// at the bottom of https://golang.org/cmd/go/#hdr-Module_proxy_protocol.
		modulePrefix = moduleVersionDir(modulePath, resolvedVersion) + "/"

		// dirs is the set of directories with at least one .go file,
		// to be populated during phase 1 and used during phase 2.
		//
		// The map key is the directory path, with the modulePrefix trimmed.
		// The map value is a slice of all .go files, and no other files.
		dirs = make(map[string][]*zip.File)

		// modInfo contains all the module information a package in the module
		// needs to render its documentation, to be populated during phase 1
		// and used during phase 2.
		modInfo = &dochtml.ModuleInfo{
			ModulePath:      modulePath,
			ResolvedVersion: resolvedVersion,
			ModulePackages:  make(map[string]bool),
		}

		// incompleteDirs tracks directories for which we have incomplete
		// information, due to a problem processing one of the go files contained
		// therein. We use this so that a single unprocessable package does not
		// prevent processing of other packages in the module.
		incompleteDirs       = make(map[string]bool)
		packageVersionStates = []*internal.PackageVersionState{}
	)

	// Phase 1.
	// Loop over zip files preemptively and check for problems
	// that can be detected by looking at metadata alone.
	// We'll be looking at file contents starting with phase 2 only,
	// only after we're sure this phase passed without errors.
	for _, f := range r.File {
		if f.Mode().IsDir() {
			// While "go mod download" will never put a directory in a zip, any can serve their
			// own zips. Example: go.felesatra.moe/binpack@v0.1.0.
			// Directory entries are harmless, so we just ignore them.
			continue
		}
		if !strings.HasPrefix(f.Name, modulePrefix) {
			// Well-formed module zips have all files under modulePrefix.
			return nil, nil, fmt.Errorf("expected file to have prefix %q; got = %q: %w",
				modulePrefix, f.Name, errMalformedZip)
		}
		innerPath := path.Dir(f.Name[len(modulePrefix):])
		if incompleteDirs[innerPath] {
			// We already know this directory cannot be processed, so skip.
			continue
		}
		importPath := path.Join(modulePath, innerPath)
		if ignoredByGoTool(importPath) || isVendored(importPath) {
			// File is in a directory we're not looking to process at this time, so skip it.
			continue
		}
		if !strings.HasSuffix(f.Name, ".go") {
			// We care about .go files only.
			continue
		}
		// It's possible to have a Go package in a directory that does not result in a valid import path.
		// That package cannot be imported, but that may be fine if it's a main package, intended to built
		// and run from that directory.
		// Example:  https://github.com/postmannen/go-learning/blob/master/concurrency/01-sending%20numbers%20and%20receving%20numbers%20from%20a%20channel/main.go
		// We're not set up to handle invalid import paths, so skip these packages.
		if err := module.CheckImportPath(importPath); err != nil {
			incompleteDirs[innerPath] = true
			packageVersionStates = append(packageVersionStates, &internal.PackageVersionState{
				ModulePath:  modulePath,
				PackagePath: importPath,
				Version:     resolvedVersion,
				Status:      derrors.ToStatus(derrors.PackageBadImportPath),
				Error:       err.Error(),
			})
			continue
		}
		if f.UncompressedSize64 > MaxFileSize {
			incompleteDirs[innerPath] = true
			status := derrors.ToStatus(derrors.PackageMaxFileSizeLimitExceeded)
			err := fmt.Sprintf("Unable to process %s: file size %d exceeds max limit %d",
				f.Name, f.UncompressedSize64, MaxFileSize)
			packageVersionStates = append(packageVersionStates, &internal.PackageVersionState{
				ModulePath:  modulePath,
				PackagePath: importPath,
				Version:     resolvedVersion,
				Status:      status,
				Error:       err,
			})
			continue
		}
		dirs[innerPath] = append(dirs[innerPath], f)
		if len(dirs) > maxPackagesPerModule {
			return nil, nil, fmt.Errorf("%d packages found in %q; exceeds limit %d for maxPackagePerModule", len(dirs), modulePath, maxPackagesPerModule)
		}
	}
	for pkgName := range dirs {
		modInfo.ModulePackages[path.Join(modulePath, pkgName)] = true
	}

	// Phase 2.
	// If we got this far, the file metadata was okay.
	// Start reading the file contents now to extract information
	// about Go packages.
	var pkgs []*internal.LegacyPackage
	for innerPath, goFiles := range dirs {
		if incompleteDirs[innerPath] {
			// Something went wrong when processing this directory, so we skip.
			log.Infof(ctx, "Skipping %q because it is incomplete", innerPath)
			continue
		}

		var (
			status error
			errMsg string
		)
		pkg, err := loadPackage(ctx, goFiles, innerPath, sourceInfo, modInfo)
		if bpe := (*BadPackageError)(nil); errors.As(err, &bpe) {
			incompleteDirs[innerPath] = true
			status = derrors.PackageInvalidContents
			errMsg = err.Error()
		} else if errors.Is(err, dochtml.ErrTooLarge) {
			status = derrors.PackageDocumentationHTMLTooLarge
			errMsg = err.Error()
		} else if err != nil {
			return nil, nil, fmt.Errorf("unexpected error loading package: %v", err)
		}

		var pkgPath string
		if pkg == nil {
			// No package.
			if len(goFiles) > 0 {
				// There were go files, but no build contexts matched them.
				incompleteDirs[innerPath] = true
				status = derrors.PackageBuildContextNotSupported
			}
			pkgPath = path.Join(modulePath, innerPath)
		} else {
			if d != nil { //  should only be nil for tests
				isRedist, lics := d.PackageInfo(innerPath)
				pkg.IsRedistributable = isRedist
				for _, l := range lics {
					pkg.Licenses = append(pkg.Licenses, l.Metadata)
				}
			}
			pkgs = append(pkgs, pkg)
			pkgPath = pkg.Path
		}
		code := http.StatusOK
		if status != nil {
			code = derrors.ToStatus(status)
		}
		packageVersionStates = append(packageVersionStates, &internal.PackageVersionState{
			ModulePath:  modulePath,
			PackagePath: pkgPath,
			Version:     resolvedVersion,
			Status:      code,
			Error:       errMsg,
		})
	}
	if len(pkgs) == 0 {
		return nil, packageVersionStates, errModuleContainsNoPackages
	}
	return pkgs, packageVersionStates, nil
}

// ignoredByGoTool reports whether the given import path corresponds
// to a directory that would be ignored by the go tool.
//
// The logic of the go tool for ignoring directories is documented at
// https://golang.org/cmd/go/#hdr-Package_lists_and_patterns:
//
// 	LegacyDirectory and file names that begin with "." or "_" are ignored
// 	by the go tool, as are directories named "testdata".
//
func ignoredByGoTool(importPath string) bool {
	for _, el := range strings.Split(importPath, "/") {
		if strings.HasPrefix(el, ".") || strings.HasPrefix(el, "_") || el == "testdata" {
			return true
		}
	}
	return false
}

// isVendored reports whether the given import path corresponds
// to a Go package that is inside a vendor directory.
//
// The logic for what is considered a vendor directory is documented at
// https://golang.org/cmd/go/#hdr-Vendor_Directories.
func isVendored(importPath string) bool {
	return strings.HasPrefix(importPath, "vendor/") ||
		strings.Contains(importPath, "/vendor/")
}

// zipContainsFilename reports whether there is a file with the given name in the zip.
func zipContainsFilename(r *zip.Reader, name string) bool {
	for _, f := range r.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

// BadPackageError represents an error loading a package
// because its contents do not make up a valid package.
//
// This can happen, for example, if the .go files fail
// to parse or declare different package names.
type BadPackageError struct {
	Err error // Not nil.
}

func (bpe *BadPackageError) Error() string { return bpe.Err.Error() }

// Go environments used to construct build contexts in loadPackage.
var goEnvs = []struct{ GOOS, GOARCH string }{
	{"linux", "amd64"},
	{"windows", "amd64"},
	{"darwin", "amd64"},
	{"js", "wasm"},
	{"linux", "js"},
}

// loadPackage loads a Go package by calling loadPackageWithBuildContext, trying
// several build contexts in turn. The first build context in the list to produce
// a non-empty package is used. If none of them result in a package, then
// loadPackage returns nil, nil.
//
// If the package is fine except that its documentation is too large, loadPackage
// returns both a package and a non-nil error with dochtml.ErrTooLarge in its chain.
func loadPackage(ctx context.Context, zipGoFiles []*zip.File, innerPath string, sourceInfo *source.Info, modInfo *dochtml.ModuleInfo) (*internal.LegacyPackage, error) {
	ctx, span := trace.StartSpan(ctx, "fetch.loadPackage")
	defer span.End()
	for _, env := range goEnvs {
		pkg, err := loadPackageWithBuildContext(ctx, env.GOOS, env.GOARCH, zipGoFiles, innerPath, sourceInfo, modInfo)
		if err != nil && !errors.Is(err, dochtml.ErrTooLarge) {
			return nil, err
		}
		if pkg != nil {
			return pkg, err
		}
	}
	return nil, nil
}

// httpPost allows package fetch tests to stub out playground URL fetches.
var httpPost = http.Post

const docTooLargeReplacement = `<p>Documentation is too large to display.</p>`

// loadPackageWithBuildContext loads a Go package made of .go files in zipGoFiles
// using a build context constructed from the given GOOS and GOARCH values.
// modulePath is stdlib.ModulePath for the Go standard library and the module
// path for all other modules. innerPath is the path of the Go package directory
// relative to the module root.
//
// zipGoFiles must contain only .go files that have been verified
// to be of reasonable size.
//
// The returned LegacyPackage.Licenses field is not populated.
//
// It returns a nil LegacyPackage if the directory doesn't contain a Go package
// or all .go files have been excluded by constraints.
// A *BadPackageError error is returned if the directory
// contains .go files but do not make up a valid package.
func loadPackageWithBuildContext(ctx context.Context, goos, goarch string, zipGoFiles []*zip.File, innerPath string, sourceInfo *source.Info, modInfo *dochtml.ModuleInfo) (_ *internal.LegacyPackage, err error) {
	modulePath := modInfo.ModulePath
	defer derrors.Wrap(&err, "loadPackageWithBuildContext(%q, %q, zipGoFiles, %q, %q, %+v)",
		goos, goarch, innerPath, modulePath, sourceInfo)
	// Apply build constraints to get a map from matching file names to their contents.
	files, err := matchingFiles(goos, goarch, zipGoFiles)
	if err != nil {
		return nil, err
	}

	// Parse .go files and add them to the goFiles slice.
	var (
		fset            = token.NewFileSet()
		goFiles         = make(map[string]*ast.File)
		allGoFiles      []*ast.File
		packageName     string
		packageNameFile string // Name of file where packageName came from.
	)
	for name, b := range files {
		pf, err := parser.ParseFile(fset, name, b, parser.ParseComments)
		if err != nil {
			if pf == nil {
				return nil, fmt.Errorf("internal error: the source couldn't be read: %v", err)
			}
			return nil, &BadPackageError{Err: err}
		}
		allGoFiles = append(allGoFiles, pf)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		goFiles[name] = pf
		if len(goFiles) == 1 {
			packageName = pf.Name.Name
			packageNameFile = name
		} else if pf.Name.Name != packageName {
			return nil, &BadPackageError{Err: &build.MultiplePackageError{
				Dir:      innerPath,
				Packages: []string{packageName, pf.Name.Name},
				Files:    []string{packageNameFile, name},
			}}
		}
	}
	if len(goFiles) == 0 {
		// This directory doesn't contain a package, or at least not one
		// that matches this build context.
		return nil, nil
	}

	// The "builtin" package in the standard library is a special case.
	// We want to show documentation for all globals (not just exported ones),
	// and avoid association of consts, vars, and factory functions with types
	// since it's not helpful (see golang.org/issue/6645).
	var noFiltering, noTypeAssociation bool
	if modulePath == stdlib.ModulePath && innerPath == "builtin" {
		noFiltering = true
		noTypeAssociation = true
	}

	// Compute package documentation.
	importPath := path.Join(modulePath, innerPath)
	var m doc.Mode
	if noFiltering {
		m |= doc.AllDecls
	}
	d, err := doc.NewFromFiles(fset, allGoFiles, importPath, m)
	if err != nil {
		return nil, fmt.Errorf("doc.NewFromFiles: %v", err)
	}
	if d.ImportPath != importPath || d.Name != packageName {
		panic(fmt.Errorf("internal error: *doc.Package has an unexpected import path (%q != %q) or package name (%q != %q)", d.ImportPath, importPath, d.Name, packageName))
	}
	if noTypeAssociation {
		for _, t := range d.Types {
			d.Consts, t.Consts = append(d.Consts, t.Consts...), nil
			d.Vars, t.Vars = append(d.Vars, t.Vars...), nil
			d.Funcs, t.Funcs = append(d.Funcs, t.Funcs...), nil
		}
		sort.Slice(d.Funcs, func(i, j int) bool { return d.Funcs[i].Name < d.Funcs[j].Name })
	}

	// Process package imports.
	if len(d.Imports) > maxImportsPerPackage {
		return nil, fmt.Errorf("%d imports found package %q; exceeds limit %d for maxImportsPerPackage", len(d.Imports), importPath, maxImportsPerPackage)
	}

	// Render documentation HTML.
	sourceLinkFunc := func(n ast.Node) string {
		if sourceInfo == nil {
			return ""
		}
		p := fset.Position(n.Pos())
		if p.Line == 0 { // invalid Position
			return ""
		}
		sourceInfoLineURL := sourceInfo.LineURL(path.Join(innerPath, p.Filename), p.Line)
		log.Info(ctx, fmt.Sprintf("sourceInfoLineURL = %v\n", sourceInfoLineURL))
		return sourceInfoLineURL
	}
	fileLinkFunc := func(filename string) string {
		if sourceInfo == nil {
			return ""
		}
		sourceInfoFileURL := sourceInfo.FileURL(path.Join(innerPath, filename))
		log.Info(ctx, fmt.Sprintf("sourceInfoFileURL = %v\n", sourceInfoFileURL))
		return sourceInfoFileURL
	}
	// This will return the Sourcegraph URL, I guess?
	// usesLinkFunc := func(defParts ...string) string {
	// 	sourcegraphBaseURL := "https://sourcegraph.com"

	// 	var def string
	// 	switch len(defParts) == "" {
	// 	case 1:

	// 	case 3:

	// 	default:
	// 		panic(fmt.Errorf("%v defParts, want 1 or 3", len(defParts)))
	// 	}

	// 	q := url.Values{
	// 		"repo": {pdoc.ProjectRoot},
	// 		"pkg":  {pdoc.ImportPath},
	// 		"def":  {def},
	// 	}

	// 	u := pdoc.sourcegraphURL + "/-/godoc/refs?" + q.Encode()
	// 	return ""
	// }
	// log.Info(ctx, fmt.Sprintf("importPath = %v\n", importPath))
	// log.Info(ctx, fmt.Sprintf("modulePath = %v\n", modulePath))
	// log.Info(ctx, fmt.Sprintf("innerPath = %v\n", innerPath))
	// usesLinkFunc := func(n ast.Node) string {
	// 	if sourceInfo == nil {
	// 		return ""
	// 	}
	// 	p :=
	// }

	docHTML, err := dochtml.Render(ctx, fset, d, dochtml.RenderOptions{
		FileLinkFunc:   fileLinkFunc,
		SourceLinkFunc: sourceLinkFunc,
		// UsesLinkFunc:   usesLinkFunc,
		ModInfo: modInfo,
		Limit:   int64(MaxDocumentationHTML),
	})
	if errors.Is(err, dochtml.ErrTooLarge) {
		docHTML = template.MustParseAndExecuteToHTML(docTooLargeReplacement)
	} else if err != nil {
		return nil, fmt.Errorf("dochtml.Render: %v", err)
	}
	if modulePath == stdlib.ModulePath {
		importPath = innerPath
	}
	v1path := internal.V1Path(importPath, modulePath)
	return &internal.LegacyPackage{
		Path:              importPath,
		Name:              packageName,
		Synopsis:          doc.Synopsis(d.Doc),
		V1Path:            v1path,
		Imports:           d.Imports,
		DocumentationHTML: docHTML,
		GOOS:              goos,
		GOARCH:            goarch,
	}, err
}

// matchingFiles returns a map from file names to their contents, read from zipGoFiles.
// It includes only those files that match the build context determined by goos and goarch.
func matchingFiles(goos, goarch string, zipGoFiles []*zip.File) (files map[string][]byte, err error) {
	defer derrors.Wrap(&err, "matchingFiles(%q, %q, zipGoFiles)", goos, goarch)
	// Populate the map with all the zip files.
	files = make(map[string][]byte)
	for _, f := range zipGoFiles {
		_, name := path.Split(f.Name)
		b, err := readZipFile(f, MaxFileSize)
		if err != nil {
			return nil, err
		}
		files[name] = b
	}

	// bctx is used to make decisions about which of the .go files are included
	// by build constraints.
	bctx := &build.Context{
		GOOS:        goos,
		GOARCH:      goarch,
		CgoEnabled:  true,
		Compiler:    build.Default.Compiler,
		ReleaseTags: build.Default.ReleaseTags,

		JoinPath: path.Join,
		OpenFile: func(name string) (io.ReadCloser, error) {
			return ioutil.NopCloser(bytes.NewReader(files[name])), nil
		},

		// If left nil, the default implementations of these read from disk,
		// which we do not want. None of these functions should be used
		// inside this function; it would be an internal error if they are.
		// Set them to non-nil values to catch if that happens.
		SplitPathList: func(string) []string { panic("internal error: unexpected call to SplitPathList") },
		IsAbsPath:     func(string) bool { panic("internal error: unexpected call to IsAbsPath") },
		IsDir:         func(string) bool { panic("internal error: unexpected call to IsDir") },
		HasSubdir:     func(string, string) (string, bool) { panic("internal error: unexpected call to HasSubdir") },
		ReadDir:       func(string) ([]os.FileInfo, error) { panic("internal error: unexpected call to ReadDir") },
	}

	for name := range files {
		match, err := bctx.MatchFile(".", name) // This will access the file we just added to files map above.
		if err != nil {
			return nil, &BadPackageError{Err: fmt.Errorf(`bctx.MatchFile(".", %q): %w`, name, err)}
		}
		if !match {
			// Excluded by build context.
			delete(files, name)
		}
	}
	return files, nil
}

// readZipFile decompresses zip file f and returns its uncompressed contents.
// The caller can check f.UncompressedSize64 before calling readZipFile to
// get the expected uncompressed size of f.
//
// limit is the maximum number of bytes to read.
func readZipFile(f *zip.File, limit int64) (_ []byte, err error) {
	defer derrors.Add(&err, "readZipFile(%q)", f.Name)

	r, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("f.Open(): %v", err)
	}
	b, err := ioutil.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("ioutil.ReadAll(r): %v", err)
	}
	if err := r.Close(); err != nil {
		return nil, fmt.Errorf("closing: %v", err)
	}
	return b, nil
}

func allocMeg() int {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int(ms.Alloc / (1024 * 1024))
}
