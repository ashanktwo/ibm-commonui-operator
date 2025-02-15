//
// Copyright 2022 IBM Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package resources

import (
	"context"
	"fmt"

	route "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	operatorsv1alpha1 "github.com/IBM/ibm-commonui-operator/api/v1alpha1"
)

const CnRouteName = "cp-console"
const CnRoutePath = "/"

var CnAnnotations = map[string]string{
	"haproxy.router.openshift.io/timeout":                               "90s",
	"haproxy.router.openshift.io/pod-concurrent-connections":            "100",
	"haproxy.router.openshift.io/rate-limit-connections":                "true",
	"haproxy.router.openshift.io/rate-limit-connections.concurrent-tcp": "100",
	"haproxy.router.openshift.io/rate-limit-connections.rate-http":      "100",
	"haproxy.router.openshift.io/rate-limit-connections.rate-tcp":       "100",
}

func ReconcileRoutes(ctx context.Context, client client.Client, instance *operatorsv1alpha1.CommonWebUI, needToRequeue *bool) error {

	reqLogger := log.WithValues("func", "ReconcileRoutes", "namespace", instance.Namespace)

	//Get the destination cert for the route
	secret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: UICertSecretName, Namespace: instance.Namespace}, secret)

	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("Unable to get route destination certificate, secret does exist. Requeue and try again", "SecretName", UICertSecretName)
			*needToRequeue = true
			return nil
		}
		reqLogger.Error(err, "Failed to get route destination certificate "+UICertSecretName)
		return err
	}
	destinationCAcert := secret.Data["ca.crt"]

	//Get the routehost from the ibmcloud-cluster-info configmap
	routeHost := ""
	clusterInfoConfigMap := &corev1.ConfigMap{}
	err = client.Get(ctx, types.NamespacedName{Name: ClusterInfoConfigmapName, Namespace: instance.Namespace}, clusterInfoConfigMap)
	if err != nil {
		if errors.IsNotFound(err) {
			//The ibmcloud-cluster-info configmap doesn't exist, reque and try again
			reqLogger.Info("Cluster info configmap was not found.  Requeue and try again", "configmapName", ClusterInfoConfigmapName)
			*needToRequeue = true
			return nil
		}

		reqLogger.Error(err, "Failed to get cluster info configmap "+ClusterInfoConfigmapName)
		return err
	}

	if clusterInfoConfigMap.Data == nil || len(clusterInfoConfigMap.Data["cluster_address"]) == 0 {
		return fmt.Errorf("cluster_address is not set in configmap %s", ClusterInfoConfigmapName)
	}

	routeHost = clusterInfoConfigMap.Data["cluster_address"]

	err = ReconcileRoute(ctx, client, instance, CnRouteName, CnAnnotations, routeHost, CnRoutePath, destinationCAcert, needToRequeue)
	if err != nil {
		return err
	}

	return nil
}

func ReconcileRoute(ctx context.Context, client client.Client, instance *operatorsv1alpha1.CommonWebUI,
	name string, annotations map[string]string, routeHost string, routePath string, destinationCAcert []byte, needToRequeue *bool) error {

	namespace := instance.Namespace
	reqLogger := log.WithValues("func", "ReconcileRoute", "name", name, "namespace", namespace)

	reqLogger.Info("Reconciling route", "annotations", annotations, "routeHost", routeHost, "routePath", routePath)

	desiredRoute, err := GetDesiredRoute(client, instance, name, namespace, annotations, routeHost, routePath, destinationCAcert)
	if err != nil {
		reqLogger.Error(err, "Error creating desired route for reconcilition")
		return err
	}

	route := &route.Route{}
	err = client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, route)
	if err != nil && !errors.IsNotFound(err) {
		reqLogger.Error(err, "Failed to get existing route for reconciliation")
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Route not found - creating")

		err = client.Create(ctx, desiredRoute)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				// Route already exists from a previous reconcile
				reqLogger.Info("Route already exists")
				*needToRequeue = true
			} else {
				// Failed to create a new route
				reqLogger.Error(err, "Failed to create new route")
				return err
			}
		} else {
			// Requeue after creating new route
			*needToRequeue = true
		}
	} else {
		// Determine if current route has changed
		reqLogger.Info("Comparing current and desired routes")

		//Update the desired route with any existing annotations specified on the existing route.  This will
		//ensure that any settings added by the customer will not be removed and any updates to existing
		//annotations are maintained ... this assumes no one will mess with the rewrite path which feels safe
		for name, annotation := range route.Annotations {
			desiredRoute.Annotations[name] = annotation
		}

		//routeHost is immutable so it must be checked first and the route recreated if it has changed
		//We have discovered that the to:service is also immutable, so we will check that as well
		if route.Spec.Host != desiredRoute.Spec.Host || route.Spec.To.Name != desiredRoute.Spec.To.Name {
			err = client.Delete(ctx, route)
			if err != nil {
				reqLogger.Error(err, "Route host or service name changed, unable to delete existing route for recreate")
				return err
			}
			//Recreate the route
			err = client.Create(ctx, desiredRoute)
			if err != nil {
				reqLogger.Error(err, "Route host or service name changed, unable to create new route")
				return err
			}
			*needToRequeue = true
			return nil
		}

		if !IsRouteEqual(route, desiredRoute) {
			reqLogger.Info("Updating route")

			route.ObjectMeta.Name = desiredRoute.ObjectMeta.Name
			route.ObjectMeta.Annotations = desiredRoute.ObjectMeta.Annotations
			route.Spec = desiredRoute.Spec

			err = client.Update(ctx, route)
			if err != nil {
				reqLogger.Error(err, "Failed to update route")
				return err
			}
		}
	}
	return nil
}

func GetDesiredRoute(client client.Client, instance *operatorsv1alpha1.CommonWebUI, name string, namespace string,
	annotations map[string]string, routeHost string, routePath string, destinationCAcert []byte) (*route.Route, error) {

	reqLogger := log.WithValues("func", "GetDesiredRoute", "name", name, "namespace", namespace)

	weight := int32(100)

	r := &route.Route{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Route",
			APIVersion: route.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels:      LabelsForMetadata(name),
		},
		Spec: route.RouteSpec{
			Host: routeHost,
			Path: routePath,
			Port: &route.RoutePort{
				TargetPort: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 3000,
				},
			},
			To: route.RouteTargetReference{
				Name:   ServiceName,
				Kind:   "Service",
				Weight: &weight,
			},
			WildcardPolicy: route.WildcardPolicyNone,
			TLS: &route.TLSConfig{
				Termination:                   route.TLSTerminationReencrypt,
				InsecureEdgeTerminationPolicy: route.InsecureEdgeTerminationPolicyRedirect,
				DestinationCACertificate:      string(destinationCAcert),
			},
		},
	}

	err := controllerutil.SetControllerReference(instance, r, client.Scheme())
	if err != nil {
		reqLogger.Error(err, "Failed to set owner for route")
		return nil, err
	}

	return r, nil
}
