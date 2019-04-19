package objectstore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/heptio/developer-dash/internal/cluster"
	"github.com/heptio/developer-dash/internal/util/retry"
	"github.com/heptio/developer-dash/pkg/objectstoreutil"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"
	authorizationv1 "k8s.io/api/authorization/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kLabels "k8s.io/apimachinery/pkg/labels"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	kcache "k8s.io/client-go/tools/cache"
)

const (
	// defaultMutableResync is the resync period for informers.
	defaultInformerResync = time.Second * 180
)

func initDynamicSharedInformerFactory(client cluster.ClientInterface) (dynamicinformer.DynamicSharedInformerFactory, error) {
	dynamicClient, err := client.DynamicClient()
	if err != nil {
		return nil, err
	}

	// TODO: make this respect namespaces instead of using cluster scope.
	// Related: https://github.com/kubernetes/kubernetes/issues/71714
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, defaultInformerResync)
	return factory, nil
}

func currentInformer(
	key objectstoreutil.Key,
	client cluster.ClientInterface,
	factory dynamicinformer.DynamicSharedInformerFactory,
	stopCh <-chan struct{}) (informers.GenericInformer, error) {
	if factory == nil {
		return nil, errors.New("dynamic shared informer factory is nil")
	}

	if client == nil {
		return nil, errors.New("cluster client is nil")
	}

	gvk := key.GroupVersionKind()

	gvr, err := client.Resource(gvk.GroupKind())
	if err != nil {
		return nil, errors.Wrap(err, "client resource")
	}

	if err := checkAccess(client, key, gvr); err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("check access: %s", gvr.Resource))
	}

	informer := factory.ForResource(gvr)
	factory.Start(stopCh)

	return informer, nil
}

func checkAccess(client cluster.ClientInterface, key objectstoreutil.Key, gvr schema.GroupVersionResource) error {
	k8sClient, err := client.KubernetesClient()
	if err != nil {
		errors.Wrap(err, "client kubernetes")
	}
	authClient := k8sClient.AuthorizationV1()

	checkVerb := func(verb string) bool {
		resourceAttributes := &authorizationv1.ResourceAttributes{
			Verb:     verb,
			Group:    gvr.Group,
			Version:  gvr.Version,
			Resource: gvr.Resource,
		}

		if key.Namespace != "" {
			// TODO: use namespace once we fix our Informer filter
			// resourceAttributes.Namespace = key.Namespace
			resourceAttributes.Namespace = metav1.NamespaceAll
		}

		sar := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: resourceAttributes,
			},
		}

		response, err := authClient.SelfSubjectAccessReviews().Create(sar)
		if err != nil {
			return false
		}

		if response.Status.Allowed {
			return true
		}
		return false
	}

	verbs := []string{"get", "list", "watch"}
	mustHaveVerbs := map[string]bool{"get": false, "list": false, "watch": false}
	for _, v := range verbs {
		mustHaveVerbs[v] = checkVerb(v)
	}

	for _, v := range mustHaveVerbs {
		if v == false {
			currentAccess := fmt.Sprintf("get: %t, list: %t, watch: %t",
				mustHaveVerbs["get"],
				mustHaveVerbs["list"],
				mustHaveVerbs["watch"],
			)
			return errors.New(fmt.Sprintf("requires cluster scoped get/list/watch access, have %s", currentAccess))
		}
	}
	return nil
}

// DynamicCacheOpt is an option for configuration DynamicCache.
type DynamicCacheOpt func(*DynamicCache)

// DynamicCache is a cache based on the dynamic shared informer factory.
type DynamicCache struct {
	initFactoryFunc func(cluster.ClientInterface) (dynamicinformer.DynamicSharedInformerFactory, error)
	factory         dynamicinformer.DynamicSharedInformerFactory
	client          cluster.ClientInterface
	stopCh          <-chan struct{}
	seenGVKs        map[schema.GroupVersionKind]bool

	mu sync.Mutex
}

var _ (ObjectStore) = (*DynamicCache)(nil)

// NewDynamicCache creates an instance of DynamicCache.
func NewDynamicCache(client cluster.ClientInterface, stopCh <-chan struct{}, options ...DynamicCacheOpt) (*DynamicCache, error) {
	c := &DynamicCache{
		initFactoryFunc: initDynamicSharedInformerFactory,
		client:          client,
		stopCh:          stopCh,
		seenGVKs:        make(map[schema.GroupVersionKind]bool),
	}

	for _, option := range options {
		option(c)
	}

	factory, err := c.initFactoryFunc(client)
	if err != nil {
		return nil, errors.Wrap(err, "initialize dynamic shared informer factory")
	}

	c.factory = factory
	return c, nil
}

type lister interface {
	List(selector kLabels.Selector) ([]kruntime.Object, error)
}

func (dc *DynamicCache) currentInformer(key objectstoreutil.Key) (informers.GenericInformer, error) {
	gvk := key.GroupVersionKind()

	informer, err := currentInformer(key, dc.client, dc.factory, dc.stopCh)
	if err != nil {
		return nil, err
	}

	dc.mu.Lock()
	defer dc.mu.Unlock()

	if _, ok := dc.seenGVKs[gvk]; ok {
		return informer, nil
	}

	ctx := context.Background()
	if !kcache.WaitForCacheSync(ctx.Done(), informer.Informer().HasSynced) {
		return nil, errors.New("shutting down")
	}

	dc.seenGVKs[gvk] = true

	return informer, nil
}

// List lists objects.
func (dc *DynamicCache) List(ctx context.Context, key objectstoreutil.Key) ([]*unstructured.Unstructured, error) {
	_, span := trace.StartSpan(ctx, "dynamicCacheList")
	defer span.End()

	span.Annotate([]trace.Attribute{
		trace.StringAttribute("namespace", key.Namespace),
		trace.StringAttribute("apiVersion", key.APIVersion),
		trace.StringAttribute("kind", key.Kind),
	}, "list key")

	informer, err := dc.currentInformer(key)
	if err != nil {
		return nil, errors.Wrapf(err, "retrieving informer for %v", key)
	}

	var l lister
	if key.Namespace == "" {
		l = informer.Lister()
	} else {
		l = informer.Lister().ByNamespace(key.Namespace)
	}

	var selector = kLabels.Everything()
	if key.Selector != nil {
		selector = key.Selector.AsSelector()
	}

	objects, err := l.List(selector)
	if err != nil {
		return nil, errors.Wrapf(err, "listing %v", key)
	}

	list := make([]*unstructured.Unstructured, len(objects))
	for i, obj := range objects {
		u, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, errors.Wrapf(err, "converting %T to unstructured", obj)
		}
		list[i] = &unstructured.Unstructured{Object: u}
	}

	return list, nil
}

type getter interface {
	Get(string) (kruntime.Object, error)
}

// Get retrieves a single object.
func (dc *DynamicCache) Get(ctx context.Context, key objectstoreutil.Key) (*unstructured.Unstructured, error) {
	_, span := trace.StartSpan(ctx, "dynamicCacheList")
	defer span.End()

	span.Annotate([]trace.Attribute{
		trace.StringAttribute("namespace", key.Namespace),
		trace.StringAttribute("apiVersion", key.APIVersion),
		trace.StringAttribute("kind", key.Kind),
		trace.StringAttribute("name", key.Name),
	}, "get key")

	informer, err := dc.currentInformer(key)
	if err != nil {
		return nil, errors.Wrapf(err, "retrieving informer for %v", key)
	}

	var g getter
	if key.Namespace == "" {
		g = informer.Lister()
	} else {
		g = informer.Lister().ByNamespace(key.Namespace)
	}

	var retryCount int64

	var object kruntime.Object
	retryErr := retry.Retry(3, time.Second, func() error {
		object, err = g.Get(key.Name)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				retryCount++
				return retry.Stop(errors.Wrap(err, "lister Get"))
			}
			return err
		}

		return nil
	})

	if retryCount > 0 {
		span.Annotate([]trace.Attribute{
			trace.Int64Attribute("retryCount", retryCount),
		}, "get retried")
	}

	if retryErr != nil {
		return nil, err
	}

	// Verify the selector matches if provided
	if key.Selector != nil {
		accessor := meta.NewAccessor()
		m, err := accessor.Labels(object)
		if err != nil {
			return nil, errors.New("retrieving labels")
		}
		labels := kLabels.Set(m)
		selector := key.Selector.AsSelector()
		if !selector.Matches(labels) {
			return nil, errors.New("object found but filtered by selector")
		}
	}

	u, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, errors.Wrapf(err, "converting %T to unstructured", object)
	}
	return &unstructured.Unstructured{Object: u}, nil
}

// Watch watches the cluster for an event and performs actions with the
// supplied handler.
func (dc *DynamicCache) Watch(key objectstoreutil.Key, handler kcache.ResourceEventHandler) error {
	informer, err := dc.currentInformer(key)
	if err != nil {
		return errors.Wrapf(err, "retrieving informer for %s", key)
	}

	informer.Informer().AddEventHandler(handler)
	return nil
}
