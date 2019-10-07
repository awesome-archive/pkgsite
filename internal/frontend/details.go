// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"strings"
	"time"

	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/derrors"
	"golang.org/x/discovery/internal/license"
	"golang.org/x/discovery/internal/log"
	"golang.org/x/discovery/internal/stdlib"
	"golang.org/x/discovery/internal/thirdparty/module"
	"golang.org/x/discovery/internal/thirdparty/semver"
	"golang.org/x/xerrors"
)

// unknownModulePath is used to indicate cases where the modulePath is
// ambiguous based on the urlPath. For example, if the urlPath is
// <path>@<version> or <path>, we cannot know for sure what part of <path> is
// the modulePath vs the packagePath suffix.
const unknownModulePath = "<unknown>"

// DetailsPage contains data for a package of module details template.
type DetailsPage struct {
	basePage
	CanShowDetails bool
	Settings       TabSettings
	Details        interface{}
	Header         interface{}
	BreadcrumbPath template.HTML
	Tabs           []TabSettings
	Namespace      string
}

// legacyHandlePackageDetails redirects all redirects to "/pkg" to "/", so that
// old url links from screenshots don't break.
//
// This will be deleted before launch.
func (s *Server) legacyHandlePackageDetails(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/pkg")
	http.Redirect(w, r, urlPath, http.StatusMovedPermanently)
}

func (s *Server) handlePackageDetails(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.staticPageHandler("index.tmpl", "Go Discovery")(w, r)
		return
	}
	if r.URL.Path == "/std" || strings.HasPrefix(r.URL.Path, "/std@v") {
		s.handleModuleDetails(w, r)
		return
	}

	pkgPath, modulePath, version, err := parseDetailsURLPath(r.URL.Path)
	if err != nil {
		log.Errorf("handleDetails: %v", err)
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}

	// Package "C" is a special case: redirect to the Go Blog article on cgo.
	// (This is what godoc.org does.)
	if pkgPath == "C" {
		http.Redirect(w, r, "https://golang.org/doc/articles/c_go_cgo.html", http.StatusMovedPermanently)
		return
	}

	var pkg *internal.VersionedPackage
	code, epage := fetchPackageOrModule(r.Context(), s.ds, "pkg", pkgPath, version, func(ver string) error {
		var err error
		if modulePath == unknownModulePath || modulePath == stdlib.ModulePath {
			pkg, err = s.ds.GetPackage(r.Context(), pkgPath, ver)
		} else {
			pkg, err = s.ds.GetPackageInModuleVersion(r.Context(), pkgPath, modulePath, ver)
		}
		return err
	})
	if code == http.StatusOK {
		s.servePackagePage(w, r, pkg)
		return
	}
	if code != http.StatusNotFound {
		s.serveErrorPage(w, r, code, epage)
		return
	}
	s.serveDirectoryPage(w, r, pkgPath, version)
}

// servePackagePage applies database data to the appropriate template.
// Handles all endpoints that match "/<import-path>[@<version>?tab=<tab>]".
func (s *Server) servePackagePage(w http.ResponseWriter, r *http.Request, pkg *internal.VersionedPackage) {
	pkgHeader, err := createPackage(&pkg.Package, &pkg.VersionInfo)
	if err != nil {
		log.Errorf("error creating package header for %s@%s: %v", pkg.Path, pkg.Version, err)
		s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
		return
	}

	tab := r.FormValue("tab")
	settings, ok := packageTabLookup[tab]
	if !ok {
		if pkg.IsRedistributable() {
			tab = "doc"
		} else {
			tab = "subdirectories"
		}
		settings = packageTabLookup[tab]
	}
	canShowDetails := pkg.IsRedistributable() || settings.AlwaysShowDetails

	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForPackage(r.Context(), r, tab, s.ds, pkg)
		if err != nil {
			log.Errorf("error fetching page for %q: %v", tab, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
	}

	page := &DetailsPage{
		basePage:       newBasePage(r, packageTitle(&pkg.Package)),
		Settings:       settings,
		Header:         pkgHeader,
		BreadcrumbPath: breadcrumbPath(pkgHeader.Path, pkgHeader.Module.Path, pkgHeader.Module.Version),
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           packageTabSettings,
		Namespace:      "pkg",
	}
	s.servePage(w, settings.TemplateName, page)
}

func constructModuleURL(modulePath, version string) string {
	url := "/"
	if modulePath != stdlib.ModulePath {
		url += "mod/"
	}
	url += modulePath
	if version != internal.LatestVersion {
		url += "@" + version
	}
	return url
}

func constructPackageURL(pkgPath, modulePath, version string) string {
	if version == internal.LatestVersion {
		return "/" + pkgPath
	}
	if pkgPath == modulePath || modulePath == stdlib.ModulePath {
		return fmt.Sprintf("/%s@%s", pkgPath, version)
	}
	return fmt.Sprintf("/%s@%s/%s", modulePath, version, strings.TrimPrefix(pkgPath, modulePath+"/"))
}

// fetchPackageOrModule handles logic common to the initial phase of
// handling both packages and modules: fetching information about the package
// or module.
// It parses urlPath into an import path and version, then calls the get
// function with those values. If get fails because the version cannot be
// found, fetchPackageOrModule calls get again with the latest version,
// to see if any versions of the package/module exist, in order to provide a
// more helpful error message.
//
// fetchPackageOrModule returns the import path and version requested, an
// HTTP status code, and possibly an error page to display.
func fetchPackageOrModule(ctx context.Context, ds DataSource, namespace, path, version string, get func(v string) error) (code int, _ *errorPage) {
	if version != internal.LatestVersion && !semver.IsValid(version) {
		// A valid semantic version was not requested.
		epage := &errorPage{Message: fmt.Sprintf("%q is not a valid semantic version.", version)}
		if namespace == "pkg" {
			epage.SecondaryMessage = suggestedSearch(path)
		}
		log.Infof("%s@%s: invalid version", path, version)
		return http.StatusBadRequest, epage
	}

	excluded, err := ds.IsExcluded(ctx, path)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	if excluded {
		// Return NotFound; don't let the user know that the package was excluded.
		return http.StatusNotFound, nil
	}

	// Fetch the package or module from the database.
	err = get(version)
	if err == nil {
		// A package or module was found for this path and version.
		return http.StatusOK, nil
	}
	log.Errorf("fetchPackageOrModule(%q, %q, %q): got error: %v",
		namespace, path, version, err)
	if !xerrors.Is(err, derrors.NotFound) {
		// Something went wrong in executing the get function.
		return http.StatusInternalServerError, nil
	}
	if version == internal.LatestVersion {
		// We were not able to find a module or package at any version.
		return http.StatusNotFound, nil
	}

	// We did not find the given version, but maybe there is another version
	// available for this package or module.
	if err := get(internal.LatestVersion); err != nil {
		log.Errorf("error: get(%s, Latest) for %s: %v", path, namespace, err)
		// Couldn't get the latest version, for whatever reason. Treat
		// this like not finding the original version.
		return http.StatusNotFound, nil
	}

	// There is a later version of this package/module.
	word := "package"
	urlPath := "/" + path
	if namespace == "mod" {
		word = "module"
		urlPath = "/mod/" + path
	}
	epage := &errorPage{
		Message: fmt.Sprintf("%s %s@%s is not available.", strings.Title(word), path, version),
		SecondaryMessage: template.HTML(
			fmt.Sprintf(`There are other versions of this %s that are! To view them, <a href="%s?tab=versions">click here</a>.</p>`, word, urlPath)),
	}
	return http.StatusSeeOther, epage
}

// fetchDetailsForPackage returns tab details by delegating to the correct detail
// handler.
func fetchDetailsForPackage(ctx context.Context, r *http.Request, tab string, ds DataSource, pkg *internal.VersionedPackage) (interface{}, error) {
	switch tab {
	case "doc":
		return fetchDocumentationDetails(ctx, ds, pkg)
	case "versions":
		return fetchPackageVersionsDetails(ctx, ds, pkg)
	case "subdirectories":
		return fetchPackageDirectoryDetails(ctx, ds, pkg.Path, &pkg.VersionInfo)
	case "imports":
		return fetchImportsDetails(ctx, ds, pkg)
	case "importedby":
		return fetchImportedByDetails(ctx, ds, pkg)
	case "licenses":
		return fetchPackageLicensesDetails(ctx, ds, pkg)
	case "readme":
		return fetchReadMeDetails(ctx, ds, &pkg.VersionInfo)
	}
	return nil, fmt.Errorf("BUG: unable to fetch details: unknown tab %q", tab)
}

// handleModuleDetails applies database data to the appropriate template.
// Handles all endpoints that match "/mod/<module-path>[@<version>?tab=<tab>]".
func (s *Server) handleModuleDetails(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/mod")
	path, _, version, err := parseDetailsURLPath(urlPath)
	if err != nil {
		log.Infof("handleModuleDetails: %v", err)
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}

	ctx := r.Context()
	var moduleVersion *internal.VersionInfo
	code, epage := fetchPackageOrModule(ctx, s.ds, "mod", path, version, func(ver string) error {
		var err error
		moduleVersion, err = s.ds.GetVersionInfo(ctx, path, ver)
		return err
	})
	if code != http.StatusOK {
		s.serveErrorPage(w, r, code, epage)
		return
	}
	// Here, moduleVersion is a valid *VersionInfo.
	licenses, err := s.ds.GetModuleLicenses(ctx, moduleVersion.ModulePath, moduleVersion.Version)
	if err != nil {
		log.Errorf("error getting module licenses for %s@%s: %v", moduleVersion.ModulePath, moduleVersion.Version, err)
		s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
		return
	}

	tab := r.FormValue("tab")
	settings, ok := moduleTabLookup[tab]
	if !ok {
		tab = "readme"
		settings = moduleTabLookup["readme"]
	}

	modHeader := createModule(moduleVersion, license.ToMetadatas(licenses))
	canShowDetails := modHeader.IsRedistributable || settings.AlwaysShowDetails
	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForModule(ctx, r, tab, s.ds, moduleVersion, licenses)
		if err != nil {
			log.Errorf("error fetching page for %q: %v", tab, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
	}
	page := &DetailsPage{
		basePage:       newBasePage(r, moduleTitle(moduleVersion.ModulePath)),
		Settings:       settings,
		Header:         modHeader,
		BreadcrumbPath: "",
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           moduleTabSettings,
		Namespace:      "mod",
	}
	s.servePage(w, settings.TemplateName, page)
}

// fetchDetailsForModule returns tab details by delegating to the correct detail
// handler.
func fetchDetailsForModule(ctx context.Context, r *http.Request, tab string, ds DataSource, vi *internal.VersionInfo, licenses []*license.License) (interface{}, error) {
	switch tab {
	case "packages":
		return fetchModuleDirectoryDetails(ctx, ds, vi)
	case "licenses":
		return &LicensesDetails{Licenses: transformLicenses(vi.ModulePath, vi.Version, licenses)}, nil
	case "versions":
		return fetchModuleVersionsDetails(ctx, ds, vi)
	case "readme":
		// TODO(b/138448402): implement remaining module views.
		return fetchReadMeDetails(ctx, ds, vi)
	}
	return nil, fmt.Errorf("BUG: unable to fetch details: unknown tab %q", tab)
}

// parseDetailsURLPath returns the modulePath (if known),
// pkgPath and version specified by urlPath.
// urlPath is assumed to be a valid path following the structure:
//   /<module-path>[@<version>/<suffix>]
//
// If <version> is not specified, internal.LatestVersion is used for the
// version. modulePath can only be determined if <version> is specified.
//
// Leading and trailing slashes in the urlPath are trimmed.
func parseDetailsURLPath(urlPath string) (pkgPath, modulePath, version string, err error) {
	defer derrors.Wrap(&err, "parseDetailsURLPath(%q)", urlPath)

	// This splits urlPath into either:
	//   /<module-path>[/<suffix>]
	// or
	//   /<module-path>, @<version>/<suffix>
	// or
	//  /<module-path>/<suffix>, @<version>
	// TODO(b/140191811) The last URL route should redirect.
	parts := strings.SplitN(urlPath, "@", 2)
	basePath := strings.TrimSuffix(strings.TrimPrefix(parts[0], "/"), "/")
	if len(parts) == 1 {
		modulePath = unknownModulePath
		version = internal.LatestVersion
		pkgPath = basePath
	} else {
		// Parse the version and suffix from parts[1].
		endParts := strings.Split(parts[1], "/")
		suffix := strings.Join(endParts[1:], "/")
		version = endParts[0]
		if suffix == "" || version == internal.LatestVersion {
			modulePath = unknownModulePath
			pkgPath = basePath
		} else {
			modulePath = basePath
			pkgPath = basePath + "/" + suffix
		}
	}
	if err := module.CheckImportPath(pkgPath); err != nil {
		return "", "", "", fmt.Errorf("malformed path %q: %v", pkgPath, err)
	}
	if inStdLib(pkgPath) {
		modulePath = stdlib.ModulePath
	}
	return pkgPath, modulePath, version, nil
}

// TabSettings defines tab-specific metadata.
type TabSettings struct {
	// Name is the tab name used in the URL.
	Name string

	// DisplayName is the formatted tab name.
	DisplayName string

	// AlwaysShowDetails defines whether the tab content can be shown even if the
	// package is not determined to be redistributable.
	AlwaysShowDetails bool

	// TemplateName is the name of the template used to render the
	// corresponding tab, as defined in Server.templates.
	TemplateName string
}

var (
	packageTabSettings = []TabSettings{
		{
			Name:         "doc",
			DisplayName:  "Doc",
			TemplateName: "pkg_doc.tmpl",
		},
		{
			Name:         "readme",
			DisplayName:  "README",
			TemplateName: "readme.tmpl",
		},
		{
			Name:              "subdirectories",
			AlwaysShowDetails: true,
			DisplayName:       "Subdirectories",
			TemplateName:      "subdirectories.tmpl",
		},
		{
			Name:              "versions",
			AlwaysShowDetails: true,
			DisplayName:       "Versions",
			TemplateName:      "versions.tmpl",
		},
		{
			Name:              "imports",
			DisplayName:       "Imports",
			AlwaysShowDetails: true,
			TemplateName:      "pkg_imports.tmpl",
		},
		{
			Name:              "importedby",
			DisplayName:       "Imported By",
			AlwaysShowDetails: true,
			TemplateName:      "pkg_importedby.tmpl",
		},
		{
			Name:         "licenses",
			DisplayName:  "Licenses",
			TemplateName: "licenses.tmpl",
		},
	}
	packageTabLookup = make(map[string]TabSettings)

	moduleTabSettings = []TabSettings{
		{
			Name:         "readme",
			DisplayName:  "README",
			TemplateName: "readme.tmpl",
		},
		{
			Name:              "packages",
			AlwaysShowDetails: true,
			DisplayName:       "Packages",
			TemplateName:      "subdirectories.tmpl",
		},
		{
			Name:              "versions",
			AlwaysShowDetails: true,
			DisplayName:       "Versions",
			TemplateName:      "versions.tmpl",
		},
		{
			Name:         "licenses",
			DisplayName:  "Licenses",
			TemplateName: "licenses.tmpl",
		},
	}
	moduleTabLookup = make(map[string]TabSettings)
)

func init() {
	for _, d := range packageTabSettings {
		packageTabLookup[d.Name] = d
	}
	for _, d := range moduleTabSettings {
		moduleTabLookup[d.Name] = d
	}
}

// Package contains information for an individual package.
type Package struct {
	Module
	Path              string
	Suffix            string
	Synopsis          string
	IsRedistributable bool
	URL               string
	Licenses          []LicenseMetadata
}

// Module contains information for an individual module.
type Module struct {
	Version           string
	Path              string
	CommitTime        string
	RepositoryURL     string
	IsRedistributable bool
	URL               string
	Licenses          []LicenseMetadata
}

// createPackage returns a *Package based on the fields of the specified
// internal package and version info.
func createPackage(pkg *internal.Package, vi *internal.VersionInfo) (_ *Package, err error) {
	defer derrors.Wrap(&err, "createPackage(%v, %v)", pkg, vi)

	if pkg == nil || vi == nil {
		return nil, fmt.Errorf("package and version info must not be nil")
	}

	suffix := strings.TrimPrefix(strings.TrimPrefix(pkg.Path, vi.ModulePath), "/")
	if suffix == "" {
		suffix = effectiveName(pkg) + " (root)"
	}

	var modLicenses []*license.Metadata
	for _, lm := range pkg.Licenses {
		if path.Dir(lm.FilePath) == "." {
			modLicenses = append(modLicenses, lm)
		}
	}

	m := createModule(vi, modLicenses)
	return &Package{
		Path:              pkg.Path,
		Suffix:            suffix,
		Synopsis:          pkg.Synopsis,
		IsRedistributable: pkg.IsRedistributable(),
		Licenses:          transformLicenseMetadata(pkg.Licenses),
		Module:            *m,
		URL:               constructPackageURL(pkg.Path, vi.ModulePath, vi.Version),
	}, nil
}

// createModule returns a *Module based on the fields of the specified
// versionInfo.
func createModule(vi *internal.VersionInfo, licmetas []*license.Metadata) *Module {
	return &Module{
		Version:           vi.Version,
		Path:              vi.ModulePath,
		CommitTime:        elapsedTime(vi.CommitTime),
		RepositoryURL:     vi.RepositoryURL,
		IsRedistributable: license.AreRedistributable(licmetas),
		Licenses:          transformLicenseMetadata(licmetas),
		URL:               constructModuleURL(vi.ModulePath, vi.Version),
	}
}

// inStdLib reports whether the package is part of the Go standard library.
func inStdLib(path string) bool {
	if i := strings.IndexByte(path, '/'); i != -1 {
		return !strings.Contains(path[:i], ".")
	}
	return !strings.Contains(path, ".")
}

// effectiveName returns either the command name or package name.
func effectiveName(pkg *internal.Package) string {
	if pkg.Name != "main" {
		return pkg.Name
	}
	var prefix string // package path without version
	if pkg.Path[len(pkg.Path)-3:] == "/v1" {
		prefix = pkg.Path[:len(pkg.Path)-3]
	} else {
		prefix, _, _ = module.SplitPathVersion(pkg.Path)
	}
	_, base := path.Split(prefix)
	return base
}

// packageTitle constructs the details page title for pkg.
// The string will appear in the <title> and <h1> element.
func packageTitle(pkg *internal.Package) string {
	if pkg.Name != "main" {
		return "Package " + pkg.Name
	}
	return "Command " + effectiveName(pkg)
}

// breadcrumbPath builds HTML that displays pkgPath as a sequence of links
// to its parents.
// pkgPath is a slash-separated path, and may be a package import path or a directory.
// modPath is the package's module path. This will be a prefix of pkgPath, except
// within the standard library.
// version is the version for the module, or LatestVersion.
//
// See TestBreadcrumbPath for examples.
func breadcrumbPath(pkgPath, modPath, version string) template.HTML {
	// Obtain successive prefixes of pkgPath, stopping at modPath,
	// or for the stdlib, at the end.
	minLen := len(modPath) - 1
	if modPath == stdlib.ModulePath {
		minLen = 1
	}
	var dirs []string
	for dir := pkgPath; len(dir) > minLen && len(path.Dir(dir)) < len(dir); dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	// Construct the path elements of the result.
	// They will be in reverse order of dirs.
	elems := make([]string, len(dirs))
	// The first dir is the current page. If it is the only one, leave it
	// as is. Otherwise, use its base. In neither case does it get a link.
	d := dirs[0]
	if len(dirs) > 1 {
		d = path.Base(d)
	}
	elems[len(elems)-1] = fmt.Sprintf(`<span class="DetailsHeader-breadcrumbCurrent">%s</span>`, d)
	// Make all the other parts into links.
	for i := 1; i < len(dirs); i++ {
		href := "/" + dirs[i]
		if version != internal.LatestVersion {
			href += "@" + version
		}
		el := dirs[i]
		if i != len(dirs)-1 {
			el = path.Base(el)
		}
		elems[len(elems)-i-1] = fmt.Sprintf(`<a href="%s">%s</a>`, template.HTMLEscapeString(href), template.HTMLEscapeString(el))
	}
	return template.HTML(`<div class="DetailsHeader-breadcrumb">` +
		strings.Join(elems, `<span class="DetailsHeader-breadcrumbDivider">/</span>`) +
		`</div>`)
}

// moduleTitle constructs the details page title for pkg.
func moduleTitle(modulePath string) string {
	if modulePath == stdlib.ModulePath {
		return "Standard library"
	}
	return "Module " + modulePath
}

// elapsedTime takes a date and returns returns human-readable,
// relative timestamps based on the following rules:
// (1) 'X hours ago' when X < 6
// (2) 'today' between 6 hours and 1 day ago
// (3) 'Y days ago' when Y < 6
// (4) A date formatted like "Jan 2, 2006" for anything further back
func elapsedTime(date time.Time) string {
	elapsedHours := int(time.Since(date).Hours())
	if elapsedHours == 1 {
		return "1 hour ago"
	} else if elapsedHours < 6 {
		return fmt.Sprintf("%d hours ago", elapsedHours)
	}

	elapsedDays := elapsedHours / 24
	if elapsedDays < 1 {
		return "today"
	} else if elapsedDays == 1 {
		return "1 day ago"
	} else if elapsedDays < 6 {
		return fmt.Sprintf("%d days ago", elapsedDays)
	}

	return date.Format("Jan _2, 2006")
}

// DocumentationDetails contains data for the doc template.
type DocumentationDetails struct {
	ModulePath    string
	Documentation template.HTML
}

// fetchDocumentationDetails fetches data for the package specified by path and version
// from the database and returns a DocumentationDetails.
func fetchDocumentationDetails(ctx context.Context, ds DataSource, pkg *internal.VersionedPackage) (*DocumentationDetails, error) {
	return &DocumentationDetails{
		ModulePath:    pkg.VersionInfo.ModulePath,
		Documentation: template.HTML(pkg.DocumentationHTML),
	}, nil
}

// fileSource returns the original filepath in the module zip where the given
// filePath can be found. For std, the corresponding URL in
// go.google.source.com/go is returned.
func fileSource(modulePath, version, filePath string) string {
	if modulePath != stdlib.ModulePath {
		return fmt.Sprintf("%s@%s/%s", modulePath, version, filePath)
	}

	root := strings.TrimPrefix(stdlib.GoRepoURL, "https://")
	tag, err := stdlib.TagForVersion(version)
	if err != nil {
		// This should never happen unless there is a bug in
		// stdlib.TagForVersion. In which case, fallback to the default
		// zipFilePath.
		log.Errorf("fileSource: %v", err)
		return fmt.Sprintf("%s/+/refs/heads/master/%s", root, filePath)
	}
	return fmt.Sprintf("%s/+/refs/tags/%s/%s", root, tag, filePath)
}
