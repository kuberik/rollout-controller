package controller

import (
	"context"
	"time"

	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kuberik/rollout-controller/internal/releasecatalogue"
)

// DefaultCatalogueDiscoveryInterval is how often a controller that started
// without the release-controller CRDs asks the API server whether they are
// served now.
const DefaultCatalogueDiscoveryInterval = 30 * time.Second

// catalogueServed reports whether the API server serves the ReleaseSource and
// ReleaseSlice kinds.
func catalogueServed(dc discovery.ServerResourcesInterface) bool {
	list, err := dc.ServerResourcesForGroupVersion(releasecatalogue.GroupVersion.String())
	if err != nil || list == nil {
		return false
	}
	found := map[string]bool{}
	for _, r := range list.APIResources {
		found[r.Kind] = true
	}
	return found["ReleaseSource"] && found["ReleaseSlice"]
}

// registerWhenCatalogueServed adds a runnable that polls discovery every
// interval until the catalogue kinds are served, then calls register once (it is
// retried on error). It is how a controller that started before release-controller
// was installed picks the catalogue up without a restart; until then Rollouts
// report ReleaseCatalogueUnavailable.
func registerWhenCatalogueServed(mgr ctrl.Manager, name string, interval time.Duration, register func(ctx context.Context) error) error {
	if interval <= 0 {
		interval = DefaultCatalogueDiscoveryInterval
	}
	dc, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	log := logf.Log.WithName(name)
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if catalogueServed(dc) {
				if err := register(ctx); err != nil {
					log.Error(err, "release catalogue is served but its watches could not be registered; will retry")
				} else {
					log.Info("release-controller CRDs (ReleaseSource, ReleaseSlice) are now served; catalogue watches registered")
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}))
}
