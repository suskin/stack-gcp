/*
Copyright 2019 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	container "google.golang.org/api/container/v1beta1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runtimev1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/test"

	"github.com/crossplane/stack-gcp/apis/container/v1beta1"
	gcpv1alpha3 "github.com/crossplane/stack-gcp/apis/v1alpha3"
	gke "github.com/crossplane/stack-gcp/pkg/clients/cluster"
)

const (
	name      = "test-cluster"
	namespace = "mynamespace"

	projectID          = "myproject-id-1234"
	providerName       = "gcp-provider"
	providerSecretName = "gcp-creds"
	providerSecretKey  = "creds"
)

var errBoom = errors.New("boom")

var _ managed.ExternalConnecter = &clusterConnector{}
var _ managed.ExternalClient = &clusterExternal{}

func gError(code int, message string) *googleapi.Error {
	return &googleapi.Error{
		Code:    code,
		Body:    "{}\n",
		Message: message,
	}
}

type clusterModifier func(*v1beta1.GKECluster)

func withConditions(c ...runtimev1alpha1.Condition) clusterModifier {
	return func(i *v1beta1.GKECluster) { i.Status.SetConditions(c...) }
}

func withProviderStatus(s string) clusterModifier {
	return func(i *v1beta1.GKECluster) { i.Status.AtProvider.Status = s }
}

func withBindingPhase(p runtimev1alpha1.BindingPhase) clusterModifier {
	return func(i *v1beta1.GKECluster) { i.Status.SetBindingPhase(p) }
}

func withLocations(l []string) clusterModifier {
	return func(i *v1beta1.GKECluster) { i.Spec.ForProvider.Locations = l }
}

func withUsername(u string) clusterModifier {
	return func(i *v1beta1.GKECluster) {
		i.Spec.ForProvider.MasterAuth = &v1beta1.MasterAuth{
			Username: &u,
		}
	}
}

func cluster(im ...clusterModifier) *v1beta1.GKECluster {
	i := &v1beta1.GKECluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Finalizers: []string{},
			Annotations: map[string]string{
				meta.ExternalNameAnnotationKey: name,
			},
		},
		Spec: v1beta1.GKEClusterSpec{
			ResourceSpec: runtimev1alpha1.ResourceSpec{
				ProviderReference: &corev1.ObjectReference{Name: providerName},
			},
			ForProvider: v1beta1.GKEClusterParameters{},
		},
	}

	for _, m := range im {
		m(i)
	}

	return i
}

func TestConnect(t *testing.T) {
	provider := gcpv1alpha3.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: providerName},
		Spec: gcpv1alpha3.ProviderSpec{
			ProjectID: projectID,
			ProviderSpec: runtimev1alpha1.ProviderSpec{
				CredentialsSecretRef: &runtimev1alpha1.SecretKeySelector{
					SecretReference: runtimev1alpha1.SecretReference{
						Namespace: namespace,
						Name:      providerSecretName,
					},
					Key: providerSecretKey,
				},
			},
		},
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: providerSecretName},
		Data:       map[string][]byte{providerSecretKey: []byte("olala")},
	}

	type args struct {
		mg resource.Managed
	}
	type want struct {
		err error
	}

	cases := map[string]struct {
		conn managed.ExternalConnecter
		args args
		want want
	}{
		"Connected": {
			conn: &clusterConnector{
				kube: &test.MockClient{MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
					switch key {
					case client.ObjectKey{Name: providerName}:
						*obj.(*gcpv1alpha3.Provider) = provider
					case client.ObjectKey{Namespace: namespace, Name: providerSecretName}:
						*obj.(*corev1.Secret) = secret
					}
					return nil
				}},
				newServiceFn: func(ctx context.Context, opts ...option.ClientOption) (*container.Service, error) {
					return &container.Service{}, nil
				},
			},
			args: args{
				mg: cluster(),
			},
			want: want{
				err: nil,
			},
		},
		"FailedToGetProvider": {
			conn: &clusterConnector{
				kube: &test.MockClient{MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
					return errBoom
				}},
			},
			args: args{
				mg: cluster(),
			},
			want: want{
				err: errors.Wrap(errBoom, errGetProvider),
			},
		},
		"FailedToGetProviderSecret": {
			conn: &clusterConnector{
				kube: &test.MockClient{MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
					switch key {
					case client.ObjectKey{Name: providerName}:
						*obj.(*gcpv1alpha3.Provider) = provider
					case client.ObjectKey{Namespace: namespace, Name: providerSecretName}:
						return errBoom
					}
					return nil
				}},
			},
			args: args{mg: cluster()},
			want: want{err: errors.Wrap(errBoom, errGetProviderSecret)},
		},
		"ProviderSecretNil": {
			conn: &nodePoolConnector{
				kube: &test.MockClient{MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
					switch key {
					case client.ObjectKey{Name: providerName}:
						nilSecretProvider := provider
						nilSecretProvider.SetCredentialsSecretReference(nil)
						*obj.(*gcpv1alpha3.Provider) = nilSecretProvider
					case client.ObjectKey{Namespace: namespace, Name: providerSecretName}:
						return errBoom
					}
					return nil
				}},
			},
			args: args{mg: nodePool()},
			want: want{err: errors.New(errProviderSecretNil)},
		},
		"FailedToCreateContainerClient": {
			conn: &clusterConnector{
				kube: &test.MockClient{MockGet: func(_ context.Context, key client.ObjectKey, obj runtime.Object) error {
					switch key {
					case client.ObjectKey{Name: providerName}:
						*obj.(*gcpv1alpha3.Provider) = provider
					case client.ObjectKey{Namespace: namespace, Name: providerSecretName}:
						*obj.(*corev1.Secret) = secret
					}
					return nil
				}},
				newServiceFn: func(_ context.Context, _ ...option.ClientOption) (*container.Service, error) { return nil, errBoom },
			},
			args: args{mg: cluster()},
			want: want{err: errors.Wrap(errBoom, errNewClient)},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tc.conn.Connect(context.Background(), tc.args.mg)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("tc.conn.Connect(...): want error != got error:\n%s", diff)
			}
		})
	}
}

func TestObserve(t *testing.T) {
	type args struct {
		mg resource.Managed
	}
	type want struct {
		mg  resource.Managed
		obs managed.ExternalObservation
		err error
	}

	cases := map[string]struct {
		handler http.Handler
		kube    client.Client
		args    args
		want    want
	}{
		"NotFound": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(&container.Cluster{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(),
				err: nil,
			},
		},
		"GetFailed": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(&container.Cluster{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(),
				err: errors.Wrap(gError(http.StatusBadRequest, ""), errGetCluster),
			},
		},
		"NotUpToDateSpecUpdateFailed": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				gc := &container.Cluster{}
				gke.GenerateCluster(name, cluster().Spec.ForProvider, gc)
				gc.Locations = []string{"loc-1"}
				_ = json.NewEncoder(w).Encode(gc)
			}),
			kube: &test.MockClient{
				MockUpdate: test.NewMockUpdateFn(errBoom),
			},
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withLocations([]string{"loc-1"})),
				err: errors.Wrap(errBoom, errManagedUpdateFailed),
			},
		},
		"Creating": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				c := &container.Cluster{}
				gke.GenerateCluster(name, cluster().Spec.ForProvider, c)
				c.Status = v1beta1.ClusterStateProvisioning
				c.MasterAuth = &container.MasterAuth{
					Username: "admin",
					Password: "admin",
				}
				_ = json.NewEncoder(w).Encode(c)
			}),
			args: args{
				mg: cluster(withUsername("admin")),
			},
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: true,
					ConnectionDetails: connectionDetails(&container.Cluster{
						Name: name,
						MasterAuth: &container.MasterAuth{
							Username: "admin",
							Password: "admin",
						},
					}),
				},
				mg: cluster(withUsername("admin"), withProviderStatus(v1beta1.ClusterStateProvisioning), withConditions(runtimev1alpha1.Creating())),
			},
		},
		"Unavailable": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				c := &container.Cluster{}
				gke.GenerateCluster(name, cluster().Spec.ForProvider, c)
				c.Status = v1beta1.ClusterStateError
				_ = json.NewEncoder(w).Encode(c)
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:    true,
					ResourceUpToDate:  true,
					ConnectionDetails: connectionDetails(&container.Cluster{}),
				},
				mg: cluster(withProviderStatus(v1beta1.ClusterStateError), withConditions(runtimev1alpha1.Unavailable())),
			},
		},
		"RunnableUnbound": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				c := &container.Cluster{}
				gke.GenerateCluster(name, cluster().Spec.ForProvider, c)
				c.Status = v1beta1.ClusterStateRunning
				_ = json.NewEncoder(w).Encode(c)
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(),
			},
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:    true,
					ResourceUpToDate:  true,
					ConnectionDetails: connectionDetails(&container.Cluster{}),
				},
				mg: cluster(
					withProviderStatus(v1beta1.ClusterStateRunning),
					withConditions(runtimev1alpha1.Available()),
					withBindingPhase(runtimev1alpha1.BindingPhaseUnbound)),
			},
		},
		"BoundUnavailable": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodGet, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				c := &container.Cluster{}
				gke.GenerateCluster(name, cluster().Spec.ForProvider, c)
				c.Status = v1beta1.ClusterStateError
				_ = json.NewEncoder(w).Encode(c)
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(
					withProviderStatus(v1beta1.ClusterStateRunning),
					withConditions(runtimev1alpha1.Available()),
					withBindingPhase(runtimev1alpha1.BindingPhaseBound),
				),
			},
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:    true,
					ResourceUpToDate:  true,
					ConnectionDetails: connectionDetails(&container.Cluster{}),
				},
				mg: cluster(
					withProviderStatus(v1beta1.ClusterStateError),
					withConditions(runtimev1alpha1.Unavailable()),
					withBindingPhase(runtimev1alpha1.BindingPhaseBound)),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			s, _ := container.NewService(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			e := clusterExternal{
				kube:      tc.kube,
				projectID: projectID,
				cluster:   s,
			}
			obs, err := e.Observe(context.Background(), tc.args.mg)
			if tc.want.err != nil && err != nil {
				// the case where our mock server returns error.
				if diff := cmp.Diff(tc.want.err.Error(), err.Error()); diff != "" {
					t.Errorf("Observe(...): want error string != got error string:\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(tc.want.err, err); diff != "" {
					t.Errorf("Observe(...): want error != got error:\n%s", diff)
				}
			}
			if diff := cmp.Diff(tc.want.obs, obs); diff != "" {
				t.Errorf("Observe(...): -want, +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.mg, tc.args.mg); diff != "" {
				t.Errorf("Observe(...): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	wantRandom := "i-want-random-data-not-this-special-string"

	type args struct {
		ctx context.Context
		mg  resource.Managed
	}
	type want struct {
		mg  resource.Managed
		cre managed.ExternalCreation
		err error
	}

	cases := map[string]struct {
		handler http.Handler
		kube    client.Client
		args    args
		want    want
	}{
		"Successful": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if diff := cmp.Diff(http.MethodPost, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				i := &container.Cluster{}
				b, err := ioutil.ReadAll(r.Body)
				if diff := cmp.Diff(err, nil); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				err = json.Unmarshal(b, i)
				if diff := cmp.Diff(err, nil); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				_ = r.Body.Close()
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg: cluster(withConditions(runtimev1alpha1.Creating())),
				cre: managed.ExternalCreation{ConnectionDetails: managed.ConnectionDetails{
					runtimev1alpha1.ResourceCredentialsSecretPasswordKey: []byte(wantRandom),
				}},
				err: nil,
			},
		},
		"SuccessfulSkipCreate": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if diff := cmp.Diff(http.MethodPost, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				i := &container.Cluster{}
				b, err := ioutil.ReadAll(r.Body)
				if diff := cmp.Diff(err, nil); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				err = json.Unmarshal(b, i)
				if diff := cmp.Diff(err, nil); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				// Return bad request for create to demonstrate that
				// http call is never made.
				w.WriteHeader(http.StatusBadRequest)
				_ = r.Body.Close()
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(withProviderStatus(v1beta1.ClusterStateProvisioning)),
			},
			want: want{
				mg: cluster(
					withConditions(runtimev1alpha1.Creating()),
					withProviderStatus(v1beta1.ClusterStateProvisioning),
				),
				cre: managed.ExternalCreation{},
				err: nil,
			},
		},
		"AlreadyExists": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodPost, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withConditions(runtimev1alpha1.Creating())),
				err: errors.Wrap(gError(http.StatusConflict, ""), errCreateCluster),
			},
		},
		"Failed": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodPost, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withConditions(runtimev1alpha1.Creating())),
				err: errors.Wrap(gError(http.StatusBadRequest, ""), errCreateCluster),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			s, _ := container.NewService(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			e := clusterExternal{
				kube:      tc.kube,
				projectID: projectID,
				cluster:   s,
			}
			_, err := e.Create(tc.args.ctx, tc.args.mg)
			if tc.want.err != nil && err != nil {
				// the case where our mock server returns error.
				if diff := cmp.Diff(tc.want.err.Error(), err.Error()); diff != "" {
					t.Errorf("Create(...): -want, +got:\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(tc.want.err, err); diff != "" {
					t.Errorf("Create(...): -want, +got:\n%s", diff)
				}
			}
			if diff := cmp.Diff(tc.want.mg, tc.args.mg); diff != "" {
				t.Errorf("Create(...): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	type args struct {
		mg resource.Managed
	}
	type want struct {
		mg  resource.Managed
		err error
	}

	cases := map[string]struct {
		handler http.Handler
		kube    client.Client
		args    args
		want    want
	}{
		"Successful": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodDelete, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withConditions(runtimev1alpha1.Deleting())),
				err: nil,
			},
		},
		"SuccessfulSkipDelete": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodDelete, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				// Return bad request for delete to demonstrate that
				// http call is never made.
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(withProviderStatus(v1beta1.ClusterStateStopping)),
			},
			want: want{
				mg: cluster(
					withConditions(runtimev1alpha1.Deleting()),
					withProviderStatus(v1beta1.ClusterStateStopping),
				),
				err: nil,
			},
		},
		"AlreadyGone": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodDelete, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withConditions(runtimev1alpha1.Deleting())),
				err: nil,
			},
		},
		"Failed": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				if diff := cmp.Diff(http.MethodDelete, r.Method); diff != "" {
					t.Errorf("r: -want, +got:\n%s", diff)
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(&container.Operation{})
			}),
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(withConditions(runtimev1alpha1.Deleting())),
				err: errors.Wrap(gError(http.StatusBadRequest, ""), errDeleteCluster),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			s, _ := container.NewService(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			e := clusterExternal{
				kube:      tc.kube,
				projectID: projectID,
				cluster:   s,
			}
			err := e.Delete(context.Background(), tc.args.mg)
			if tc.want.err != nil && err != nil {
				// the case where our mock server returns error.
				if diff := cmp.Diff(tc.want.err.Error(), err.Error()); diff != "" {
					t.Errorf("Delete(...): -want, +got:\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(tc.want.err, err); diff != "" {
					t.Errorf("Delete(...): -want, +got:\n%s", diff)
				}
			}
			if diff := cmp.Diff(tc.want.mg, tc.args.mg); diff != "" {
				t.Errorf("Delete(...): -want, +got:\n%s", diff)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	type args struct {
		mg resource.Managed
	}
	type want struct {
		mg  resource.Managed
		upd managed.ExternalUpdate
		err error
	}

	cases := map[string]struct {
		handler http.Handler
		kube    client.Client
		args    args
		want    want
	}{
		"Successful": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Cluster{})
				case http.MethodPut:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(withLocations([]string{"loc-1"})),
			},
			want: want{
				mg:  cluster(withLocations([]string{"loc-1"})),
				err: nil,
			},
		},
		"SuccessfulSkipUpdateReconciling": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					// Return bad request for get to demonstrate that
					// http call is never made.
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				case http.MethodPut:
					// Return bad request for put to demonstrate that
					// http call is never made.
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(
					withLocations([]string{"loc-1"}),
					withProviderStatus(v1beta1.ClusterStateReconciling),
				),
			},
			want: want{
				mg: cluster(
					withLocations([]string{"loc-1"}),
					withProviderStatus(v1beta1.ClusterStateReconciling),
				),
				err: nil,
			},
		},
		"SuccessfulSkipUpdateProvisioning": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					// Return bad request for get to demonstrate that
					// http call is never made.
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				case http.MethodPut:
					// Return bad request for put to demonstrate that
					// http call is never made.
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(
					withLocations([]string{"loc-1"}),
					withProviderStatus(v1beta1.ClusterStateProvisioning),
				),
			},
			want: want{
				mg: cluster(
					withLocations([]string{"loc-1"}),
					withProviderStatus(v1beta1.ClusterStateProvisioning),
				),
				err: nil,
			},
		},
		"SuccessfulNoopUpdate": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Cluster{})
				case http.MethodPut:
					// Return bad request for update to demonstrate that
					// underlying update is not making any http call.
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				mg: cluster(),
			},
			want: want{
				mg:  cluster(),
				err: nil,
			},
		},
		"GetFails": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Cluster{})
				case http.MethodPut:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				// No need to actually require an update. We won't get that far.
				mg: cluster(),
			},
			want: want{
				mg:  cluster(),
				err: errors.Wrap(gError(http.StatusBadRequest, ""), errGetCluster),
			},
		},
		"UpdateFails": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.Body.Close()
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(http.StatusOK)
					// Must return successful get of cluster that does not match spec.
					_ = json.NewEncoder(w).Encode(&container.Cluster{})
				case http.MethodPut:
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				default:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(&container.Operation{})
				}
			}),
			kube: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
			},
			args: args{
				// Must include field that causes update.
				mg: cluster(withLocations([]string{"loc-1"})),
			},
			want: want{
				mg:  cluster(withLocations([]string{"loc-1"})),
				err: errors.Wrap(gError(http.StatusBadRequest, ""), errUpdateCluster),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			s, _ := container.NewService(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			e := clusterExternal{
				kube:      tc.kube,
				projectID: projectID,
				cluster:   s,
			}
			upd, err := e.Update(context.Background(), tc.args.mg)
			if tc.want.err != nil && err != nil {
				// the case where our mock server returns error.
				if diff := cmp.Diff(tc.want.err.Error(), err.Error()); diff != "" {
					t.Errorf("Update(...): -want, +got:\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(tc.want.err, err); diff != "" {
					t.Errorf("Update(...): -want, +got:\n%s", diff)
				}
			}
			if tc.want.err == nil {
				if diff := cmp.Diff(tc.want.mg, tc.args.mg); diff != "" {
					t.Errorf("Update(...): -want, +got:\n%s", diff)
				}
				if diff := cmp.Diff(tc.want.upd, upd); diff != "" {
					t.Errorf("Update(...): -want, +got:\n%s", diff)
				}
			}

		})
	}
}

func TestConnectionDetails(t *testing.T) {
	name := "gke-cluster"
	endpoint := "endpoint"
	username := "username"
	password := "password"
	clusterCA, _ := base64.StdEncoding.DecodeString("clusterCA")
	clientCert, _ := base64.StdEncoding.DecodeString("clientCert")
	clientKey, _ := base64.StdEncoding.DecodeString("clientKey")
	server := fmt.Sprintf("https://%s", endpoint)
	rawConfig :=
		`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: clusterC
    server: https://endpoint
  name: gke-cluster
contexts:
- context:
    cluster: gke-cluster
    user: gke-cluster
  name: gke-cluster
current-context: gke-cluster
kind: Config
preferences: {}
users:
- name: gke-cluster
  user:
    client-certificate-data: clientCe
    client-key-data: clientKe
    password: password
    username: username
`

	cases := map[string]struct {
		args *container.Cluster
		want managed.ConnectionDetails
	}{
		"Full": {
			args: &container.Cluster{
				Name:     name,
				Endpoint: endpoint,
				MasterAuth: &container.MasterAuth{
					Username:             username,
					Password:             password,
					ClusterCaCertificate: base64.StdEncoding.EncodeToString(clusterCA),
					ClientCertificate:    base64.StdEncoding.EncodeToString(clientCert),
					ClientKey:            base64.StdEncoding.EncodeToString(clientKey),
				},
			},
			want: map[string][]byte{
				runtimev1alpha1.ResourceCredentialsSecretEndpointKey:   []byte(server),
				runtimev1alpha1.ResourceCredentialsSecretUserKey:       []byte(username),
				runtimev1alpha1.ResourceCredentialsSecretPasswordKey:   []byte(password),
				runtimev1alpha1.ResourceCredentialsSecretCAKey:         clusterCA,
				runtimev1alpha1.ResourceCredentialsSecretClientCertKey: clientCert,
				runtimev1alpha1.ResourceCredentialsSecretClientKeyKey:  clientKey,
				runtimev1alpha1.ResourceCredentialsSecretKubeconfigKey: []byte(rawConfig),
			},
		},
		"Empty": {
			args: &container.Cluster{},
			want: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := connectionDetails(tc.args)
			if diff := cmp.Diff(tc.want, d); diff != "" {
				t.Errorf("connectionDetails(...): -want, +got:\n%s", diff)
			}
		})
	}
}
