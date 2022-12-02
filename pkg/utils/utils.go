package utils

import (
	"context"
	"strconv"
	"strings"

	"github.com/christianh814/mta/pkg/argo"
	helmv2 "github.com/fluxcd/helm-controller/api/v2beta1"
	yaml "sigs.k8s.io/yaml"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluxcd/flux2/pkg/log"
	fluxuninstall "github.com/fluxcd/flux2/pkg/uninstall"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// MigrateKustomizationToApplicationSet migrates a Kustomization to an Argo CD ApplicationSet
func MigrateKustomizationToApplicationSet(client client.Client, ctx context.Context, ans string, k kustomizev1.Kustomization) error {
	// Get the GitRepository from the Kustomization
	// get the gitsource
	gitSource := &sourcev1.GitRepository{}
	err := client.Get(ctx, types.NamespacedName{Namespace: k.Namespace, Name: k.Name}, gitSource)
	if err != nil {
		return err
	}

	//Get the secret holding the info we need
	//secret, err := _.CoreV1().Secrets(k.Namespace).Get(ctx, gitSource.Spec.SecretRef.Name, v1.GetOptions{})
	secret := &apiv1.Secret{}
	err = client.Get(ctx, types.NamespacedName{Namespace: k.Namespace, Name: gitSource.Spec.SecretRef.Name}, secret)
	if err != nil {
		return err
	}

	//Argo CD ApplicationSet is sensitive about how you give it paths in the Git Dir generator. We need to figure some things out
	var sourcePath string
	var sourcePathExclude string

	spl := strings.SplitAfter(k.Spec.Path, "./")

	if len(spl[1]) == 0 {
		sourcePath = `*`
		sourcePathExclude = "flux-system"
	} else {
		sourcePath = spl[1] + "/*"
		sourcePathExclude = spl[1] + "/flux-system"
	}

	// Generate the ApplicationSet manifest based on the struct
	applicationSet := argo.GitDirApplicationSet{
		Namespace:               ans,
		GitRepoURL:              gitSource.Spec.URL,
		GitRepoRevision:         gitSource.Spec.Reference.Branch,
		GitIncludeDir:           sourcePath,
		GitExcludeDir:           sourcePathExclude,
		AppName:                 "{{path.basename}}",
		AppProject:              "default",
		AppRepoURL:              gitSource.Spec.URL,
		AppTargetRevision:       gitSource.Spec.Reference.Branch,
		AppPath:                 "{{path}}",
		AppDestinationServer:    "https://kubernetes.default.svc",
		AppDestinationNamespace: k.Spec.TargetNamespace,
		SSHPrivateKey:           string(secret.Data["identity"]),
		GitOpsRepo:              gitSource.Spec.URL,
	}

	appset, err := argo.GenGitDirAppSet(applicationSet)
	if err != nil {
		return err
	}

	// Generate the ApplicationSet Secret and set the GVK
	appsetSecret := GenK8SSecret(applicationSet)

	// Create the Application on the cluster
	// Suspend reconcilation
	k.Spec.Suspend = true
	client.Update(ctx, &k)

	// Finally, create the Argo CD Application
	if err := CreateK8SObjects(client, ctx, appsetSecret, appset); err != nil {
		return err
	}

	// If we're here, it should have gone okay...
	return nil
}

// GenK8SSecret generates a kubernetes secret using a clientset
func GenK8SSecret(a argo.GitDirApplicationSet) *apiv1.Secret {
	// Some Defaults
	// TODO: Make these configurable
	sName := "mta-migration"
	sLabels := map[string]string{
		"argocd.argoproj.io/secret-type": "repository",
	}

	sData := map[string]string{
		"sshPrivateKey": a.SSHPrivateKey,
		"type":          "git",
		"url":           a.GitOpsRepo,
	}

	// Create the secret
	s := &apiv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sName,
			Namespace: a.Namespace,
			Labels:    sLabels,
		},
		Type:       apiv1.SecretTypeOpaque,
		StringData: sData,
	}

	// set the gvk for the secret
	s.SetGroupVersionKind(apiv1.SchemeGroupVersion.WithKind("Secret"))

	// Return the secret
	return s

}

// MigrateHelmReleaseToApplication migrates a HelmRelease to an Argo CD Application
func MigrateHelmReleaseToApplication(client client.Client, ctx context.Context, ans string, h helmv2.HelmRelease) error {
	// Get the helmchart based on type, report if error
	helmRepo := &sourcev1.HelmRepository{}
	err := client.Get(ctx, types.NamespacedName{Namespace: h.Namespace, Name: h.Spec.Chart.Spec.SourceRef.Name}, helmRepo)
	if err != nil {
		return err
	}
	// Get the Values from the HelmRelease
	yaml, err := yaml.Marshal(h.Spec.Values)
	if err != nil {
		return err
	}

	// Generate the Argo CD Helm Application
	helmApp := argo.ArgoCdHelmApplication{
		//Name:                 helmRelease.Spec.Chart.Spec.Chart + "-" + helmRelease.Name,
		Name:                 h.Name,
		Namespace:            ans,
		DestinationNamespace: h.Spec.TargetNamespace,
		DestinationServer:    "https://kubernetes.default.svc",
		Project:              "default",
		HelmChart:            h.Spec.Chart.Spec.Chart,
		HelmRepo:             helmRepo.Spec.URL,
		HelmTargetRevision:   h.Spec.Chart.Spec.Version,
		HelmValues:           string(yaml),
		HelmCreateNamespace:  strconv.FormatBool(h.Spec.Install.CreateNamespace),
	}

	helmArgoCdApp, err := argo.GenArgoCdHelmApplication(helmApp)
	if err != nil {
		return err
	}

	// Create the Application on the cluster
	// Suspend reconcilation
	h.Spec.Suspend = true
	client.Update(ctx, &h)

	// Finally, create the Argo CD Application
	if err := CreateK8SObjects(client, ctx, helmArgoCdApp); err != nil {
		return err
	}

	// If we're here, it should have gone okay...
	return nil
}

// FluxCleanUp cleans up flux resources
func FluxCleanUp(k client.Client, ctx context.Context, log log.Logger, ns string) error {
	//Set up the flux uninstall options
	// TODO: Maybe make these configurable
	uninstallFlags := struct {
		keepNamespace bool
		dryRun        bool
		silent        bool
	}{
		keepNamespace: false,
		dryRun:        false,
		silent:        false,
	}

	// Uninstall the components
	if err := fluxuninstall.Components(ctx, log, k, ns, uninstallFlags.dryRun); err != nil {
		return err
	}

	// Uninstall the finalizers
	if err := fluxuninstall.Finalizers(ctx, log, k, uninstallFlags.dryRun); err != nil {
		return err
	}

	// Uninstall CRDS
	if err := fluxuninstall.CustomResourceDefinitions(ctx, log, k, uninstallFlags.dryRun); err != nil {
		return err
	}

	// Uninstall the namespace
	if err := fluxuninstall.Namespace(ctx, log, k, ns, uninstallFlags.dryRun); err != nil {
		return err
	}

	// If we're here, it should have gone okay...
	return nil
}

// CreateK8SObjects Creates Kubernetes Objects on the Cluster based on the schema passed in the client.
func CreateK8SObjects(c client.Client, ctx context.Context, obj ...client.Object) error {
	// Migrate the objects
	for _, o := range obj {
		if err := c.Create(ctx, o); err != nil {
			return err
		}
	}

	// If we're here, it should have gone okay...
	return nil
}
