package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/addons/values"
	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// customcatalog.go implements per-user custom add-on catalogs - the tenant-facing counterpart to the
// platform's built-in catalog (internal/catalog). A CustomCatalog is owned by a user and shared
// exactly like a cluster: the owner and admins have full access, and group-mates share access via
// their per-group read/write role (Read = view, Write = edit). See internal/domain.CustomCatalog.
//
// Selecting a custom add-on onto a cluster copies its chart definition into the per-cluster
// domain.Addon (self-contained), so the untenanted reconcile loop never resolves a custom catalog -
// see resolveCustomAddons in app.go.

// CustomCatalogView is a catalog plus the presentational context the portal needs: the owner's
// username (denormalized for display) and the requesting actor's access level ("view" or "edit").
type CustomCatalogView struct {
	domain.CustomCatalog
	OwnerUsername string `json:"owner_username"`
	Access        string `json:"access"` // "view" (read-only) | "edit" (may modify)
}

// accessToOwned resolves an actor's access to a resource owned by ownerID, using the same
// owner/admin/group-role model as clusters (see accessTo). Factored out so both clusters and custom
// catalogs share one authorization rule.
func (a *App) accessToOwned(actor *domain.User, ownerID string) clusterAccess {
	if actor == nil {
		return accessNone
	}
	if actor.IsAdmin || ownerID == actor.ID {
		return accessFull
	}
	if len(actor.Memberships) == 0 {
		return accessNone
	}
	owner, err := a.Store.GetUser(ownerID)
	if err != nil {
		return accessNone
	}
	best := accessNone
	for _, m := range actor.Memberships {
		if !owner.InGroup(m.GroupID) {
			continue
		}
		if m.Role == domain.GroupRoleWrite {
			return accessFull
		}
		best = accessView
	}
	return best
}

// accessString renders a clusterAccess as the portal-facing label ("edit"/"view"/"").
func accessString(acc clusterAccess) string {
	switch acc {
	case accessFull:
		return "edit"
	case accessView:
		return "view"
	default:
		return ""
	}
}

// authorizeCatalog loads a catalog and enforces read (view) access - owner, admin, or any group-mate
// of the owner, whatever their role. An invisible catalog returns store.ErrNotFound (as with
// clusters) so a tenant can't probe for others' catalogs.
func (a *App) authorizeCatalog(actor *domain.User, id string) (*domain.CustomCatalog, error) {
	cc, err := a.Store.GetCustomCatalog(id)
	if err != nil {
		return nil, err
	}
	if a.accessToOwned(actor, cc.OwnerID) == accessNone {
		return nil, store.ErrNotFound
	}
	return cc, nil
}

// authorizeCatalogWrite loads a catalog and enforces write (edit) access - owner, admin, or a
// write-role group-mate. A read-role group-mate gets ErrForbidden (they can see it, so the honest 403
// explains the refusal); an invisible catalog is store.ErrNotFound.
func (a *App) authorizeCatalogWrite(actor *domain.User, id string) (*domain.CustomCatalog, error) {
	cc, err := a.Store.GetCustomCatalog(id)
	if err != nil {
		return nil, err
	}
	switch a.accessToOwned(actor, cc.OwnerID) {
	case accessFull:
		return cc, nil
	case accessView:
		return nil, ErrForbidden
	default:
		return nil, store.ErrNotFound
	}
}

// ListCustomCatalogs returns the catalogs the actor can see - their own plus any owned by a group-mate
// - each annotated with the owner's username and the actor's access level. Admins see all.
func (a *App) ListCustomCatalogs(actor *domain.User) ([]CustomCatalogView, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	all, err := a.Store.ListCustomCatalogs()
	if err != nil {
		return nil, err
	}
	usernames, err := a.usernamesByID()
	if err != nil {
		return nil, err
	}
	views := make([]CustomCatalogView, 0, len(all))
	for _, cc := range all {
		acc := a.accessToOwned(actor, cc.OwnerID)
		if acc == accessNone {
			continue
		}
		views = append(views, CustomCatalogView{
			CustomCatalog: *cc,
			OwnerUsername: usernames[cc.OwnerID],
			Access:        accessString(acc),
		})
	}
	return views, nil
}

// usernamesByID returns an owner-ID→username map for denormalizing catalog ownership in views.
func (a *App) usernamesByID() (map[string]string, error) {
	users, err := a.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.ID] = u.Username
	}
	return m, nil
}

// GetCustomCatalog returns one catalog (with its add-ons) the actor can see, as a view.
func (a *App) GetCustomCatalog(actor *domain.User, id string) (*CustomCatalogView, error) {
	cc, err := a.authorizeCatalog(actor, id)
	if err != nil {
		return nil, err
	}
	usernames, err := a.usernamesByID()
	if err != nil {
		return nil, err
	}
	return &CustomCatalogView{
		CustomCatalog: *cc,
		OwnerUsername: usernames[cc.OwnerID],
		Access:        accessString(a.accessToOwned(actor, cc.OwnerID)),
	}, nil
}

// CreateCustomCatalog creates an empty catalog owned by actor. Names are unique per owner.
func (a *App) CreateCustomCatalog(actor *domain.User, name string) (*domain.CustomCatalog, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	name, err := validateCatalogName(name)
	if err != nil {
		return nil, err
	}
	cc := &domain.CustomCatalog{
		ID:        newID(),
		OwnerID:   actor.ID,
		Name:      name,
		CreatedAt: time.Now(),
	}
	if err := a.Store.CreateCustomCatalog(cc); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("you already have a catalog named %q", name)
		}
		return nil, err
	}
	return cc, nil
}

// RenameCustomCatalog renames a catalog (write access required).
func (a *App) RenameCustomCatalog(actor *domain.User, id, name string) (*domain.CustomCatalog, error) {
	cc, err := a.authorizeCatalogWrite(actor, id)
	if err != nil {
		return nil, err
	}
	name, err = validateCatalogName(name)
	if err != nil {
		return nil, err
	}
	cc.Name = name
	if err := a.Store.UpdateCustomCatalog(cc); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("a catalog named %q already exists", name)
		}
		return nil, err
	}
	return cc, nil
}

// DeleteCustomCatalog deletes a catalog (write access required). Clusters that already installed its
// add-ons are unaffected - each carries a self-contained copy (see resolveCustomAddons), mirroring
// how deleting a group only drops memberships.
func (a *App) DeleteCustomCatalog(actor *domain.User, id string) error {
	if _, err := a.authorizeCatalogWrite(actor, id); err != nil {
		return err
	}
	return a.Store.DeleteCustomCatalog(id)
}

// AddCustomAddon appends a new add-on to a catalog (write access required). The add-on name is
// unique within the catalog.
func (a *App) AddCustomAddon(actor *domain.User, catalogID string, addon domain.CustomAddon) (*domain.CustomCatalog, error) {
	cc, err := a.authorizeCatalogWrite(actor, catalogID)
	if err != nil {
		return nil, err
	}
	addon, err = validateCustomAddon(addon)
	if err != nil {
		return nil, err
	}
	for _, existing := range cc.Addons {
		if existing.Name == addon.Name {
			return nil, fmt.Errorf("catalog already has an add-on named %q", addon.Name)
		}
	}
	cc.Addons = append(cc.Addons, addon)
	if err := a.Store.UpdateCustomCatalog(cc); err != nil {
		return nil, err
	}
	return cc, nil
}

// UpdateCustomAddon replaces an existing add-on in a catalog, keyed by its current name (write access
// required). The name may not change here - remove and re-add to rename.
func (a *App) UpdateCustomAddon(actor *domain.User, catalogID, name string, addon domain.CustomAddon) (*domain.CustomCatalog, error) {
	cc, err := a.authorizeCatalogWrite(actor, catalogID)
	if err != nil {
		return nil, err
	}
	addon.Name = name // the path name is authoritative; renaming is remove + add
	addon, err = validateCustomAddon(addon)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range cc.Addons {
		if cc.Addons[i].Name == name {
			cc.Addons[i] = addon
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("add-on %q is not in this catalog", name)
	}
	if err := a.Store.UpdateCustomCatalog(cc); err != nil {
		return nil, err
	}
	return cc, nil
}

// RemoveCustomAddon drops an add-on from a catalog (write access required).
func (a *App) RemoveCustomAddon(actor *domain.User, catalogID, name string) (*domain.CustomCatalog, error) {
	cc, err := a.authorizeCatalogWrite(actor, catalogID)
	if err != nil {
		return nil, err
	}
	kept := cc.Addons[:0:0]
	for _, ad := range cc.Addons {
		if ad.Name != name {
			kept = append(kept, ad)
		}
	}
	if len(kept) == len(cc.Addons) {
		return nil, fmt.Errorf("add-on %q is not in this catalog", name)
	}
	cc.Addons = kept
	if err := a.Store.UpdateCustomCatalog(cc); err != nil {
		return nil, err
	}
	return cc, nil
}

// FetchChartValues fetches a Helm chart's default values.yaml for the given repo/chart/version, so
// the authoring editor can seed from real defaults. In real mode this runs `helm show values`, which
// doubles as URL/chart validation (an unreachable repo or unknown chart surfaces as an error). Any
// authenticated user may call it - it touches no cluster or tenant state, only the public chart repo.
func (a *App) FetchChartValues(ctx context.Context, actor *domain.User, repo, chart, version string) (string, error) {
	if actor == nil {
		return "", ErrForbidden
	}
	chart = strings.TrimSpace(chart)
	if chart == "" {
		return "", fmt.Errorf("chart is required")
	}
	entry := catalog.Addon{
		Name:    chart,
		Chart:   chart,
		Repo:    strings.TrimSpace(repo),
		Version: strings.TrimSpace(version),
	}
	return a.Values.Defaults(ctx, entry)
}

// resolveCustomAddons turns the requested (catalog, add-on) references into self-contained
// per-cluster domain.Addon records: it verifies the actor can see each catalog, copies the add-on's
// chart definition and values onto the record, and rejects name collisions with the built-in add-ons
// already resolved for the cluster and across the custom refs (a helm release name must be unique per
// cluster). Custom add-ons install after the built-in ones (they may depend on platform add-ons like
// the monitoring stack's CRDs).
func (a *App) resolveCustomAddons(actor *domain.User, refs []domain.CustomAddonRef, taken map[string]bool) ([]domain.Addon, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]domain.Addon, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		cc, err := a.authorizeCatalog(actor, ref.CatalogID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("custom catalog not found or not accessible")
			}
			return nil, err
		}
		var def *domain.CustomAddon
		for i := range cc.Addons {
			if cc.Addons[i].Name == ref.Name {
				def = &cc.Addons[i]
				break
			}
		}
		if def == nil {
			return nil, fmt.Errorf("add-on %q is not in catalog %q", ref.Name, cc.Name)
		}
		if seen[def.Name] {
			continue // same custom add-on picked twice - idempotent
		}
		if taken[def.Name] {
			return nil, fmt.Errorf("add-on name %q collides with a built-in add-on already on the cluster", def.Name)
		}
		seen[def.Name] = true
		out = append(out, domain.Addon{
			Name:           def.Name,
			Version:        def.Version,
			Phase:          "pending",
			CatalogID:      cc.ID,
			Chart:          def.Chart,
			Repo:           def.Repo,
			Namespace:      def.Namespace,
			Description:    def.Description,
			ValuesOverride: def.Values,
		})
	}
	return out, nil
}

// validateCatalogName normalizes and bounds a custom catalog display name.
func validateCatalogName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 2 || len(name) > 40 {
		return "", fmt.Errorf("catalog name must be 2–40 characters")
	}
	return name, nil
}

// validateCustomAddon normalizes and checks an add-on definition: a DNS-label name (also the helm
// release name), a chart + version, and a repo XOR an oci:// chart. Values, if present, must be valid
// YAML (reusing the same check the built-in add-on editor applies).
func validateCustomAddon(a domain.CustomAddon) (domain.CustomAddon, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.Chart = strings.TrimSpace(a.Chart)
	a.Repo = strings.TrimSpace(a.Repo)
	a.Version = strings.TrimSpace(a.Version)
	a.Namespace = strings.TrimSpace(a.Namespace)
	a.Description = strings.TrimSpace(a.Description)

	if err := validateDNSLabel(a.Name, "add-on name"); err != nil {
		return a, err
	}
	if a.Namespace != "" {
		if err := validateDNSLabel(a.Namespace, "namespace"); err != nil {
			return a, err
		}
	}
	if a.Chart == "" {
		return a, fmt.Errorf("chart is required")
	}
	if a.Version == "" {
		return a, fmt.Errorf("version is required")
	}
	oci := strings.HasPrefix(a.Chart, "oci://")
	if oci && a.Repo != "" {
		return a, fmt.Errorf("an oci:// chart carries its registry in the chart ref - leave repo empty")
	}
	if !oci && a.Repo == "" {
		return a, fmt.Errorf("repo URL is required for a classic chart (or use an oci:// chart ref)")
	}
	if a.Repo != "" && !strings.HasPrefix(a.Repo, "http://") && !strings.HasPrefix(a.Repo, "https://") {
		return a, fmt.Errorf("repo must be an http(s) URL")
	}
	if strings.TrimSpace(a.Values) != "" {
		if err := values.Valid(a.Values); err != nil {
			return a, fmt.Errorf("values: %w", err)
		}
	}
	return a, nil
}

// validateDNSLabel enforces an RFC 1123 label (lowercase alnum + '-', 1–53 chars), which a helm
// release name and a Kubernetes namespace both require.
func validateDNSLabel(s, field string) error {
	if len(s) < 1 || len(s) > 53 {
		return fmt.Errorf("%s must be 1–53 characters", field)
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return fmt.Errorf("%s may contain only lowercase letters, digits, and '-'", field)
		}
		if r == '-' && (i == 0 || i == len(s)-1) {
			return fmt.Errorf("%s must not start or end with '-'", field)
		}
	}
	return nil
}
