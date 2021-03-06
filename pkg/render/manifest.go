package render

import (
	"bytes"
	"context"
	"text/template"

	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/go-utils/errors"
	"github.com/solo-io/go-utils/installutils"
	"github.com/solo-io/go-utils/installutils/helmchart"
	"github.com/solo-io/go-utils/installutils/kuberesource"
	hubv1 "github.com/solo-io/service-mesh-hub/api/v1"
	"go.uber.org/zap"
)

var (
	MissingInstallSpecError = errors.Errorf("missing installation spec")

	FailedToRenderManifestsError = func(err error) error {
		return errors.Wrapf(err, "error rendering manifests")
	}

	FailedToConvertManifestsError = func(err error) error {
		return errors.Wrapf(err, "error converting manifests to raw resources")
	}

	FailedRenderValueTemplatesError = func(err error) error {
		return errors.Wrapf(err, "error rendering input value templates")
	}
)

type SuperglooInfo struct {
	Namespace          string
	ServiceAccountName string
	ClusterRoleName    string
}

type ValuesInputs struct {
	Name               string
	InstallNamespace   string
	FlavorName         string
	MeshRef            core.ResourceRef
	SuperglooNamespace string

	UserDefinedValues string
	FlavorParams      map[string]string
	SpecDefinedValues string

	// TODO: remove old value (SuperglooNamespace) after new ones have been wired on the marketplace side
	Supergloo SuperglooInfo
}

func ComputeResourcesForApplication(ctx context.Context, inputs ValuesInputs, spec *hubv1.VersionedApplicationSpec) (kuberesource.UnstructuredResources, error) {
	inputs, err := ExecInputValuesTemplates(inputs)
	if err != nil {
		return nil, FailedRenderValueTemplatesError(err)
	}

	manifests, err := GetManifestsFromApplicationSpec(ctx, inputs, spec)
	if err != nil {
		return nil, err
	}

	installedFlavor, err := GetInstalledFlavor(inputs.FlavorName, spec.Flavors)
	if err != nil {
		return nil, err
	}

	rawResources, err := ApplyLayers(ctx, inputs, installedFlavor, manifests)
	if err != nil {
		return nil, err
	}

	return FilterByLabel(ctx, spec, rawResources), nil
}

/*
 Coalesces spec values yaml, params, and user-defined values yaml.
 User defined values override params which override spec values.
 If there is an error parsing, it is logged and propagated.
*/
func ComputeValueOverrides(ctx context.Context, inputs ValuesInputs) (string, error) {
	specValues, err := ConvertYamlStringToNestedMap(inputs.SpecDefinedValues)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing spec values yaml",
			zap.Error(err),
			zap.String("values", inputs.SpecDefinedValues))
		return "", err
	}

	paramValues, err := ConvertParamsToNestedMap(inputs.FlavorParams)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing install params",
			zap.Error(err),
			zap.Any("params", inputs.FlavorParams))
		return "", err
	}

	userValues, err := ConvertYamlStringToNestedMap(inputs.UserDefinedValues)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw("Error parsing user values yaml",
			zap.Error(err),
			zap.Any("params", inputs.UserDefinedValues))
		return "", err
	}

	valuesMap := CoalesceValuesMap(ctx, specValues, paramValues)
	valuesMap = CoalesceValuesMap(ctx, valuesMap, userValues)
	values, err := ConvertNestedMapToYaml(valuesMap)
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorw(err.Error(), zap.Error(err), zap.Any("valuesMap", valuesMap))
		return "", err
	}
	return values, nil
}

func GetManifestsFromApplicationSpec(ctx context.Context, inputs ValuesInputs, spec *hubv1.VersionedApplicationSpec) (helmchart.Manifests, error) {
	var manifests helmchart.Manifests
	switch spec.GetInstallationSpec().(type) {
	case *hubv1.VersionedApplicationSpec_GithubChart:
		githubManifests, err := getManifestsFromGithub(ctx, spec, inputs)
		if err != nil {
			return nil, err
		}
		manifests = githubManifests
	case *hubv1.VersionedApplicationSpec_HelmArchive:
		helmManifests, err := getManifestsFromHelm(ctx, spec, inputs)
		if err != nil {
			return nil, err
		}
		manifests = helmManifests
	case *hubv1.VersionedApplicationSpec_ManifestsArchive:
		archiveManifests, err := getManifestsFromArchive(ctx, spec, inputs)
		if err != nil {
			return nil, err
		}
		manifests = archiveManifests
	default:
		return nil, MissingInstallSpecError
	}

	return manifests, nil
}

func GetResourcesFromManifests(ctx context.Context, manifests helmchart.Manifests) (kuberesource.UnstructuredResources, error) {
	rawResources, err := manifests.ResourceList()
	if err != nil {
		wrapped := FailedToConvertManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err))
		return nil, wrapped
	}
	return rawResources, nil
}

func FilterByLabel(ctx context.Context, spec *hubv1.VersionedApplicationSpec, resources kuberesource.UnstructuredResources) kuberesource.UnstructuredResources {
	labels := spec.GetRequiredLabels()
	if len(labels) > 0 {
		contextutils.LoggerFrom(ctx).Infow("Filtering installed resources by label", zap.Any("labels", labels))
		return resources.WithLabels(labels)
	}
	return resources
}

func getManifestsFromHelm(ctx context.Context, spec *hubv1.VersionedApplicationSpec, inputs ValuesInputs) (helmchart.Manifests, error) {
	helmInstallSpec := spec.GetHelmArchive()
	values, err := ComputeValueOverrides(ctx, inputs)
	if err != nil {
		return nil, err
	}
	contextutils.LoggerFrom(ctx).Infow("Rendering with values", zap.String("values", values))
	manifests, err := helmchart.RenderManifests(ctx,
		helmInstallSpec.Uri,
		values,
		inputs.Name,
		inputs.InstallNamespace,
		"")
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.String("chartUri", helmInstallSpec.Uri),
			zap.String("values", values),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace),
			zap.String("kubeVersion", ""))
		return nil, wrapped
	}
	return manifests, nil
}

func getManifestsFromGithub(ctx context.Context, spec *hubv1.VersionedApplicationSpec, inputs ValuesInputs) (helmchart.Manifests, error) {
	githubInstallSpec := spec.GetGithubChart()
	ref := helmchart.GithubChartRef{
		Owner:          githubInstallSpec.Org,
		Repo:           githubInstallSpec.Repo,
		Ref:            githubInstallSpec.Ref,
		ChartDirectory: githubInstallSpec.Directory,
	}
	values, err := ComputeValueOverrides(ctx, inputs)
	if err != nil {
		return nil, err
	}
	manifests, err := helmchart.RenderManifestsFromGithub(ctx, ref,
		values,
		inputs.Name,
		inputs.InstallNamespace,
		"")
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.Any("ref", ref),
			zap.String("values", values),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace),
			zap.String("kubeVersion", ""))
		return nil, wrapped
	}
	return manifests, nil
}

func getManifestsFromArchive(ctx context.Context, spec *hubv1.VersionedApplicationSpec, inputs ValuesInputs) (helmchart.Manifests, error) {
	manifestsArchive := spec.GetManifestsArchive()
	manifests, err := installutils.GetManifestsFromRemoteTar(manifestsArchive.GetUri())
	if err != nil {
		wrapped := FailedToRenderManifestsError(err)
		contextutils.LoggerFrom(ctx).Errorw(wrapped.Error(),
			zap.Error(err),
			zap.String("manifestsArchiveUrl", manifestsArchive.GetUri()),
			zap.String("releaseName", inputs.Name),
			zap.String("namespace", inputs.InstallNamespace))
		return nil, wrapped
	}
	return manifests, nil
}

// The SpecDefinedValues, UserDefinedValues, and FlavorParams inputs can contain template
// actions (text delimited by "{{" and "}}" ). This function renders the contents of these
// parameters using the data contained in 'input' and updates 'input' with the results.
func ExecInputValuesTemplates(inputs ValuesInputs) (ValuesInputs, error) {

	// Render the helm values string that comes from the extension spec
	buf := new(bytes.Buffer)
	tpl := template.Must(template.New("specValues").Parse(inputs.SpecDefinedValues))
	if err := tpl.Execute(buf, inputs); err != nil {
		return ValuesInputs{}, err
	}
	inputs.SpecDefinedValues = buf.String()
	buf.Reset()

	// Render the helm values string that comes from the user provided overrides
	tpl = template.Must(template.New("userValues").Parse(inputs.UserDefinedValues))
	if err := tpl.Execute(buf, inputs); err != nil {
		return ValuesInputs{}, err
	}
	inputs.UserDefinedValues = buf.String()
	buf.Reset()

	// Render the values of the flavor parameters
	for paramName, paramValue := range inputs.FlavorParams {
		t := template.Must(template.New(paramName).Parse(paramValue))
		if err := t.Execute(buf, inputs); err != nil {
			return ValuesInputs{}, err
		}
		inputs.FlavorParams[paramName] = buf.String()
		buf.Reset()
	}

	return inputs, nil
}
