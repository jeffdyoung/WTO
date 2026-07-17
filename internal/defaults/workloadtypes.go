package defaults

import (
	"context"
	"embed"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
)

//go:embed workloadtypes/*.yaml
var workloadTypeFS embed.FS

func EnsureDefaults(ctx context.Context, c client.Client) error {
	log := ctrl.Log.WithName("defaults")

	entries, err := workloadTypeFS.ReadDir("workloadtypes")
	if err != nil {
		return fmt.Errorf("reading embedded workloadtypes: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		data, err := workloadTypeFS.ReadFile("workloadtypes/" + entry.Name())
		if err != nil {
			log.Error(err, "reading embedded file", "file", entry.Name())
			continue
		}

		wtc := &wtov1alpha1.WorkloadTypeConfig{}
		if err := yaml.UnmarshalStrict(data, wtc); err != nil {
			log.Error(err, "parsing embedded WorkloadTypeConfig", "file", entry.Name())
			continue
		}

		existing := &wtov1alpha1.WorkloadTypeConfig{}
		err = c.Get(ctx, types.NamespacedName{Name: wtc.Name}, existing)
		if errors.IsNotFound(err) {
			if err := c.Create(ctx, wtc); err != nil {
				log.Error(err, "creating default WorkloadTypeConfig", "name", wtc.Name)
				continue
			}
			log.Info("created default WorkloadTypeConfig", "name", wtc.Name)
			continue
		}
		if err != nil {
			log.Error(err, "checking existing WorkloadTypeConfig", "name", wtc.Name)
			continue
		}

		labels := existing.GetLabels()
		if labels["workload-template.io/managed-by"] != "wto-defaults" {
			log.Info("skipping user-managed WorkloadTypeConfig", "name", wtc.Name)
		}
	}

	return nil
}
