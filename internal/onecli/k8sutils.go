package onecli

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/kubectl/pkg/scheme"
)

type K8sClients struct {
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface
}

// FromGVKtoGVR converts Group Version Kind to Group Version Resource
func FromGVKtoGVR(discoveryClient discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	a, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	return a.Resource, nil
}

// GetResource returns a given resource from the cluster
func GetResource(gvr schema.GroupVersionResource, clients *K8sClients, res Resource) (*unstructured.Unstructured, error) {
	return clients.dynamic.Resource(gvr).
		Namespace(res.Object.GetNamespace()).
		Get(context.Background(), res.Object.GetName(), metav1.GetOptions{})
}

// CreateResource creates a K8S resource on the cluster if it does not exist
func CreateResource(gvr schema.GroupVersionResource, clients *K8sClients, res Resource) error {
	fmt.Printf("Creating %s: %s\n", res.Object.GetKind(), res.Object.GetName())

	originAnn := res.Object.GetAnnotations()
	if originAnn == nil {
		originAnn = make(map[string]string)
	}
	objJSON, err := res.Object.MarshalJSON()
	if err != nil {
		return err
	}
	originAnn[corev1.LastAppliedConfigAnnotation] = string(objJSON)
	res.Object.SetAnnotations(originAnn)

	resourceInterface := clients.dynamic.Resource(gvr).Namespace(res.Object.GetNamespace())

	_, err = resourceInterface.Create(context.Background(), &res.Object, metav1.CreateOptions{})
	return err
}

// PatchResource patches an existing resource on the cluster
func PatchResource(gvr schema.GroupVersionResource, clients *K8sClients, res Resource, onClusterObj *unstructured.Unstructured) error {
	// create the patch
	patch, patchType, err := createPatch(*onClusterObj, res)
	if err != nil {
		return errors.Wrap(err, "failed to create patch")
	}

	resourceInterface := clients.dynamic.Resource(gvr).Namespace(res.Object.GetNamespace())

	_, err = resourceInterface.Patch(context.Background(), res.Object.GetName(), patchType, patch, metav1.PatchOptions{})
	return err
}

// createPatch is a helper function that creates the patch to deploy
func createPatch(currentObj unstructured.Unstructured, target Resource) ([]byte, types.PatchType, error) {
	// Get the resource in the cluster
	currentJSON, err := currentObj.MarshalJSON()
	if err != nil {
		return nil, "", errors.Wrap(err, "serializing live configuration")
	}

	// Get last applied config from annotation if exists
	lastAppliedConfigAnnotation := ""
	lastAppliedConfigAnnotationFound := false
	var targetJSON []byte
	annotations := currentObj.GetAnnotations()
	if annotations != nil {
		lastAppliedConfigAnnotation, lastAppliedConfigAnnotationFound = annotations[corev1.LastAppliedConfigAnnotation]
	}

	if lastAppliedConfigAnnotationFound {
		annotatedTarget, err := annotateWithLastApplied(target)
		if err != nil {
			return nil, "", err
		}
		targetJSON, err = annotatedTarget.MarshalJSON()
		if err != nil {
			return nil, "", err
		}
	} else {
		targetJSON, err = target.Object.MarshalJSON()
		if err != nil {
			return nil, "", err
		}
	}

	versionedObject, err := scheme.Scheme.New(*target.GroupVersionKind)
	if err != nil && !runtime.IsNotRegisteredError(err) {
		return nil, "", err
	}

	patchMeta, err := strategicpatch.NewPatchMetaFromStruct(versionedObject)
	if err != nil {
		return nil, types.StrategicMergePatchType, errors.Wrap(err, "unable to create patch metadata from object")
	}

	patch, err := strategicpatch.CreateThreeWayMergePatch([]byte(lastAppliedConfigAnnotation), targetJSON, currentJSON, patchMeta, true)
	return patch, types.StrategicMergePatchType, err
}

// ensureNamespaceExistence is a helper function that checks the existence of a namespace
// and creates it if it does not exist
func ensureNamespaceExistence(clients *K8sClients, namespace string) error {
	ns := &unstructured.Unstructured{}
	ns.SetUnstructuredContent(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": namespace,
		},
	})

	fmt.Printf("Creating namespace %s\n", namespace)
	if _, err := clients.dynamic.Resource(gvrNamespaces).Create(context.Background(), ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

// annotateWithLastApplied handles the lastAppliedConfiguration annotation
func annotateWithLastApplied(res Resource) (unstructured.Unstructured, error) {
	annotatedRes := res.Object.DeepCopy()
	annotations := annotatedRes.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	if _, found := annotations[corev1.LastAppliedConfigAnnotation]; found {
		delete(annotations, corev1.LastAppliedConfigAnnotation)
		annotatedRes.SetAnnotations(annotations)
	}

	resJSON, err := annotatedRes.MarshalJSON()
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	annotations[corev1.LastAppliedConfigAnnotation] = string(resJSON)
	annotatedRes.SetAnnotations(annotations)

	return *annotatedRes, nil
}