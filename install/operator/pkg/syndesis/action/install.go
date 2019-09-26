package action

import (
	"context"
	"errors"
	"fmt"
	"github.com/syndesisio/syndesis/install/operator/pkg/openshift/serviceaccount"
	"os"
	"time"

	"github.com/mcuadros/go-version"
	"github.com/syndesisio/syndesis/install/operator/pkg/generator"
	"github.com/syndesisio/syndesis/install/operator/pkg/util"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8s.io/client-go/kubernetes"

	v1 "github.com/openshift/api/route/v1"

	"github.com/syndesisio/syndesis/install/operator/pkg/apis/syndesis/v1alpha1"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/configuration"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/operation"
	syndesistemplate "github.com/syndesisio/syndesis/install/operator/pkg/syndesis/template"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	SyndesisRouteName  = "syndesis"
	SyndesisPullSecret = "syndesis-pull-secret"
)

// Install syndesis into the namespace, taking resources from the bundled template.
type installAction struct {
	baseAction
}

func newInstallAction(mgr manager.Manager, api kubernetes.Interface) SyndesisOperatorAction {
	return &installAction{
		newBaseAction(mgr, api, "install"),
	}
}

func (a *installAction) CanExecute(syndesis *v1alpha1.Syndesis) bool {
	return syndesisPhaseIs(syndesis,
		v1alpha1.SyndesisPhaseInstalling,
		v1alpha1.SyndesisPhaseInstalled,
		v1alpha1.SyndesisPhaseStarting,
		v1alpha1.SyndesisPhaseStartupFailed,
	)
}

var kindsReportedNotAvailable = map[schema.GroupVersionKind]time.Time{}

func (a *installAction) Execute(ctx context.Context, originalSyndesis *v1alpha1.Syndesis) error {
	syndesis := originalSyndesis.DeepCopy()
	if syndesisPhaseIs(syndesis, v1alpha1.SyndesisPhaseInstalling) {
		a.log.Info("Installing Syndesis resource", "name", syndesis.Name)
	}
	resourcesThatShouldExist := map[types.UID]bool{}

	// Check if an image secret exists, to be used to connect to registries that require authentication
	secret := &corev1.Secret{}
	err := a.client.Get(ctx, types.NamespacedName{Namespace: syndesis.Namespace, Name: SyndesisPullSecret}, secret)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			secret = nil
		} else {
			return err
		}
	}

	serviceAccount, err := installServiceAccount(ctx, a.client, syndesis, secret)
	if err != nil {
		return err
	}
	resourcesThatShouldExist[serviceAccount.GetUID()] = true

	// Detect if the route should be auto-generated
	autoGenerateRoute := syndesis.Spec.RouteHostname == ""
	if autoGenerateRoute {
		syndesis.Spec.RouteHostname = "dummy"
	}

	config, err := configuration.GetSyndesisEnvVarsFromOpenShiftNamespace(ctx, a.client, syndesis.Namespace)
	if err != nil {
		config = map[string]string{}
	}

	token, err := serviceaccount.GetServiceAccountToken(ctx, a.client, serviceAccount.Name, syndesis.Namespace)
	if err != nil {
		return err
	}
	config[string(configuration.EnvOpenShiftOauthClientSecret)] = token

	// Handle an external database being defined
	externalDbSecName := syndesis.Spec.Components.Db.ExternalDBSecret
	if externalDbSecName != "" {
		externalDbSec := &corev1.Secret{}
		if err := a.client.Get(ctx, types.NamespacedName{Name: externalDbSecName, Namespace: syndesis.Namespace}, externalDbSec); err != nil {
			return err
		}
		config[string(configuration.EnvPostgresqlURL)] = string(externalDbSec.Data["URL"])
		config[string(configuration.EnvPostgresqlUser)] = string(externalDbSec.Data["USERNAME"])
		config[string(configuration.EnvPostgresqlDatabase)] = string(externalDbSec.Data["DATABASE"])
		config[string(configuration.EnvPostgresqlPassword)] = string(externalDbSec.Data["PASSWORD"])
	} else {
		config[string(configuration.EnvPostgresqlUser)] = syndesis.Spec.Components.Db.User
		config[string(configuration.EnvPostgresqlDatabase)] = syndesis.Spec.Components.Db.Database
	}

	renderContext, err := syndesistemplate.GetTemplateContext()
	if err != nil {
		return err
	}

	err = syndesistemplate.SetupRenderContext(renderContext, syndesis, config)
	if err != nil {
		return err
	}

	if secret != nil {
		renderContext.ImagePullSecrets = append(renderContext.ImagePullSecrets, secret.Name)
	}
	configuration.SetConfigurationFromEnvVars(renderContext.Env, syndesis)

	err = checkTags(renderContext)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

	// Render the route resource...
	all, err := generator.RenderDir("./route/", renderContext)
	if err != nil {
		return err
	}

	routes, _ := util.SeperateStructuredAndUnstructured(a.scheme, all)
	syndesisRoute, err := installSyndesisRoute(ctx, a.client, syndesis, routes, autoGenerateRoute)
	if err != nil {
		return err
	}

	resourcesThatShouldExist[syndesisRoute.GetUID()] = true

	if autoGenerateRoute {
		// Set the right hostname after generating the route
		syndesis.Spec.RouteHostname = syndesisRoute.Spec.Host

		// Hack to remove the auto-generated annotation
		// In OpenShift 3.9, the route gets low priority for being displayed as main route for the app if the openshift.io/host.generated=true annotation is present
		err = removeAutoGeneratedAnnotation(ctx, a.client, syndesisRoute)
		if err != nil {
			return err
		}
	}

	// Render the remaining syndesis resources...
	all, err = generator.RenderDir("./infrastructure/", renderContext)
	if err != nil {
		return err
	}

	// Render the database resource if needed...
	if syndesis.Spec.Components.Db.ExternalDBSecret == "" {
		dbResources, err := generator.RenderDir("./database/", renderContext)
		if err != nil {
			return err
		}
		all = append(all, dbResources...)
	}

	for addon, properties := range syndesis.Spec.Addons {
		if properties["enabled"] != "true" {
			continue
		}

		addonDir := "./addons/" + addon + "/"
		f, err := generator.GetAssetsFS().Open(addonDir)
		if err != nil {
			a.log.Info("unsuported addon configured", "addon", addon, "error", err)
			continue
		}
		f.Close()

		resources, err := generator.RenderDir(addonDir, renderContext)
		if err != nil {
			return err
		}

		all = append(all, resources...)
	}

	// Link the image secret to service accounts
	if secret != nil {
		err = linkImageSecretToServiceAccounts(ctx, a.client, syndesis, secret)
		if err != nil {
			return err
		}
	}

	// Install the resources..
	for _, res := range all {

		operation.SetNamespaceAndOwnerReference(res, syndesis)
		o, modificationType, err := util.CreateOrUpdate(ctx, a.client, &res)
		if err != nil {
			if util.IsNoKindMatchError(err) {
				gvk := res.GroupVersionKind()
				if _, found := kindsReportedNotAvailable[gvk]; !found {
					kindsReportedNotAvailable[gvk] = time.Now()
					a.log.Info("optional custom resource definition is not installed.", "group", gvk.Group, "version", gvk.Version, "kind", gvk.Kind)
				}
			} else {
				a.log.Info("Failed to create or replace resource", "kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
				return err
			}
		} else {
			resourcesThatShouldExist[o.GetUID()] = true
			if modificationType != controllerutil.OperationResultNone {
				a.log.Info("resource "+string(modificationType), "kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
			}
		}

	}

	// Find resources which need to be deleted.
	labelSelector, err := labels.Parse("owner=" + string(syndesis.GetUID()))
	if err != nil {
		panic(err)
	}
	options := client.ListOptions{
		Namespace:     syndesis.Namespace,
		LabelSelector: labelSelector,
	}
	err = ListAllTypesInChunks(ctx, a.api, a.client, options, func(list []unstructured.Unstructured) error {
		for _, res := range list {
			if resourcesThatShouldExist[res.GetUID()] {
				continue
			}
			if res.GetOwnerReferences() == nil || len(res.GetOwnerReferences()) == 0 {
				continue
			}
			if res.GetOwnerReferences()[0].UID != syndesis.GetUID() {
				continue
			}

			// Found a resource that should not exist!
			err := a.client.Delete(ctx, &res)
			if err != nil {
				if !k8serrors.IsNotFound(err) {
					a.log.Error(err, "could not deleted", "kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
				}
			} else {
				a.log.Info("resource deleted", "kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	addRouteAnnotation(originalSyndesis, syndesisRoute)
	if syndesis.Status.Phase == v1alpha1.SyndesisPhaseInstalling {
		// Installation completed, set the next state
		originalSyndesis.Status.Phase = v1alpha1.SyndesisPhaseStarting
		originalSyndesis.Status.Reason = v1alpha1.SyndesisStatusReasonMissing
		originalSyndesis.Status.Description = ""
		_, _, err := util.CreateOrUpdate(ctx, a.client, originalSyndesis, "kind", "apiVersion")
		if err != nil {
			return err
		}
		a.log.Info("Syndesis resource installed", "name", originalSyndesis.Name)
	}
	return err
}

// Checks that the tags from syndesis components is valid between the supported versions
func checkTags(context *generator.Context) error {
	c := version.NewConstrainGroupFromString(fmt.Sprintf(">=%s,<%s", context.TagMinor, context.TagMajor))
	var images = []struct {
		name string
		tag  string
	}{
		{"server", util.TagOf(context.Syndesis.Spec.Components.Server.Image)},
		{"meta", util.TagOf(context.Syndesis.Spec.Components.Meta.Image)},
		{"ui", util.TagOf(context.Syndesis.Spec.Components.UI.Image)},
		{"s2i", util.TagOf(context.Syndesis.Spec.Components.S2I.Image)},
	}
	for _, image := range images {
		if image.tag != "latest" {
			if !c.Match(version.Normalize(image.tag)) {
				return fmt.Errorf("tag for %s[%s] component is not valid, should have a value between [%s] and [%s]",
					image.name,
					image.tag,
					context.TagMinor,
					context.TagMajor)
			}
		}
	}

	return nil
}

func ListAllTypesInChunks(ctx context.Context, api kubernetes.Interface, c client.Client, options client.ListOptions, handler func([]unstructured.Unstructured) error) error {
	types, err := getTypes(api)
	if err != nil {
		return err
	}
nextType:
	for _, t := range types {
		options := client.ListOptions{
			Namespace:     options.Namespace,
			LabelSelector: options.LabelSelector,
			Raw: &metav1.ListOptions{
				TypeMeta: t,
				Limit:    200,
			},
		}
		list := unstructured.UnstructuredList{
			Object: map[string]interface{}{
				"apiVersion": t.APIVersion,
				"kind":       t.Kind,
			},
		}
		err = util.ListInChunks(ctx, c, &options, &list, handler)
		if err != nil {
			if k8serrors.IsNotFound(err) ||
				k8serrors.IsForbidden(err) ||
				k8serrors.IsMethodNotSupported(err) {
				continue nextType
			}
			return err
		}
	}
	return nil
}

func installServiceAccount(ctx context.Context, cl client.Client, syndesis *v1alpha1.Syndesis, secret *corev1.Secret) (*corev1.ServiceAccount, error) {
	sa := newSyndesisServiceAccount()
	if secret != nil {
		linkImagePullSecret(sa, secret)
	}

	operation.SetNamespaceAndOwnerReference(sa, syndesis)
	// We don't replace the service account if already present, to let Kubernetes generate its tokens
	o, _, err := util.CreateOrUpdate(ctx, cl, sa)
	if err != nil {
		return nil, err
	}
	sa.SetUID(o.GetUID())
	return sa, nil
}

func newSyndesisServiceAccount() *corev1.ServiceAccount {
	sa := corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "syndesis-oauth-client",
			Labels: map[string]string{
				"app": "syndesis",
			},
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirecturi.local":       "https://localhost:4200",
				"serviceaccounts.openshift.io/oauth-redirecturi.route":       "https://",
				"serviceaccounts.openshift.io/oauth-redirectreference.route": `{"kind": "OAuthRedirectReference", "apiVersion": "v1", "reference": {"kind": "Route","name": "syndesis"}}`,
			},
		},
	}

	return &sa
}

func addRouteAnnotation(syndesis *v1alpha1.Syndesis, route *v1.Route) {
	annotations := syndesis.ObjectMeta.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
		syndesis.ObjectMeta.Annotations = annotations
	}
	annotations["syndesis.io/applicationUrl"] = extractApplicationUrl(route)
}
func extractApplicationUrl(route *v1.Route) string {
	scheme := "http"
	if route.Spec.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + route.Spec.Host
}

func installSyndesisRoute(ctx context.Context, cl client.Client, syndesis *v1alpha1.Syndesis, objects []runtime.Object, autoGenerate bool) (*v1.Route, error) {
	route, err := findSyndesisRoute(objects)
	if err != nil {
		return nil, err
	}

	operation.SetNamespaceAndOwnerReference(route, syndesis)

	if autoGenerate {
		route.Spec.Host = ""
	}

	// We don't replace the route if already present, to let OpenShift generate its host
	o, _, err := util.CreateOrUpdate(ctx, cl, route)
	if err != nil {
		return nil, err
	}
	route.SetUID(o.GetUID())

	if route.Spec.Host != "" {
		return route, nil
	}

	// Let's try to get the route from OpenShift to check the host field
	var key client.ObjectKey
	if key, err = client.ObjectKeyFromObject(route); err != nil {
		return nil, err
	}
	err = cl.Get(ctx, key, route)
	if err != nil {
		return nil, err
	}

	if route.Spec.Host == "" {
		return nil, errors.New("hostname still not present on syndesis route")
	}
	return route, nil
}

func findSyndesisRoute(resources []runtime.Object) (*v1.Route, error) {
	for _, res := range resources {
		if route, ok := isSyndesisRoute(res); ok {
			return route, nil
		}
	}
	return nil, errors.New("syndesis route not found")
}

func isSyndesisRoute(resource runtime.Object) (*v1.Route, bool) {
	if route, ok := resource.(*v1.Route); ok {
		if route.Name == SyndesisRouteName {
			return route, true
		}
	}
	return nil, false
}

func linkImageSecretToServiceAccounts(ctx context.Context, cl client.Client, syndesis *v1alpha1.Syndesis, secret *corev1.Secret) error {
	// Link the builder service account to the image pull/push secret if it exists
	builder := &corev1.ServiceAccount{}
	err := cl.Get(ctx, types.NamespacedName{Namespace: syndesis.Namespace, Name: "builder"}, builder)
	if err != nil {
		return err
	}
	linked := linkImagePullSecret(builder, secret)
	linked = linkSecret(builder, secret.Name) || linked
	if linked {
		err = cl.Update(ctx, builder)
		if err != nil {
			return err
		}
	}
	return nil
}

func linkImagePullSecret(sa *corev1.ServiceAccount, secret *corev1.Secret) bool {
	exist := false
	for _, s := range sa.ImagePullSecrets {
		if s.Name == secret.Name {
			exist = true
			break
		}
	}

	if !exist {
		sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{
			Name: secret.Name,
		})
		return true
	}

	return false
}

func linkSecret(sa *corev1.ServiceAccount, secret string) bool {
	exist := false
	for _, s := range sa.Secrets {
		if s.Name == secret {
			exist = true
			break
		}
	}

	if !exist {
		sa.Secrets = append(sa.Secrets, corev1.ObjectReference{Namespace: sa.Namespace, Name: SyndesisPullSecret})
		return true
	}

	return false
}

func removeAutoGeneratedAnnotation(ctx context.Context, cl client.Client, route *v1.Route) error {
	return updateOnLatestRevision(ctx, cl, route, func(obj runtime.Object) {
		if r, ok := obj.(*v1.Route); ok {
			delete(r.Annotations, "openshift.io/host.generated")
		}
	})
}
