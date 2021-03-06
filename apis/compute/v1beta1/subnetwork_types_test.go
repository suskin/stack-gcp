/*
Copyright 2020 The Crossplane Authors.

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

package v1beta1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"

	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/test"
)

var _ resource.AttributeReferencer = (*NetworkURIReferencerForSubnetwork)(nil)

func TestNetworkURIReferencerForSubnetwork_AssignInvalidType_ReturnsErr(t *testing.T) {

	r := &NetworkURIReferencerForSubnetwork{}
	expectedErr := errors.New(errResourceIsNotSubnetwork)

	err := r.Assign(nil, "mockValue")
	if diff := cmp.Diff(expectedErr, err, test.EquateErrors()); diff != "" {
		t.Errorf("Assign(...): -want error, +got error:\n%s", diff)
	}
}

func TestNetworkURIReferencerForSubnetwork_AssignValidType_ReturnsExpected(t *testing.T) {
	mv := "mockValue"
	r := &NetworkURIReferencerForSubnetwork{}
	res := &Subnetwork{}
	var expectedErr error

	err := r.Assign(res, mv)
	if diff := cmp.Diff(expectedErr, err, test.EquateErrors()); diff != "" {
		t.Errorf("Assign(...): -want error, +got error:\n%s", diff)
	}

	if diff := cmp.Diff(res.Spec.ForProvider.Network, &mv); diff != "" {
		t.Errorf("Assign(...): -want value, +got value:\n%s", diff)
	}
}
