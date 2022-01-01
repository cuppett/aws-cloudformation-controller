/*
MIT License

Copyright (c) 2018 Martin Linkhorst
Copyright (c) 2022 Stephen Cuppett

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package controllers

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"reflect"
	"sync"
	"time"

	cfTypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/cuppett/aws-cloudformation-controller/api/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StackFollower ensures a Stack object is monitored until it reaches a terminal state
type StackFollower struct {
	client.Client
	ChannelHub
	Log                  logr.Logger
	CloudFormationHelper *CloudFormationHelper
	StacksFollowing      prometheus.Gauge
	StacksFollowed       prometheus.Counter
	mapPollingList       sync.Map // StackID -> Kube Stack object
}

func (f *StackFollower) Receiver() {

	for {
		toBeFollowed := <-f.ChannelHub.FollowChannel
		f.Log.Info("Received follow request", "UID", toBeFollowed.UID, "Stack ID", toBeFollowed.Status.StackID)
		if !f.BeingFollowed(toBeFollowed.Status.StackID) {
			f.startFollowing(toBeFollowed)
		}
	}
}

// BeingFollowed Identify if the follower is actively working this one.
func (f *StackFollower) BeingFollowed(stackId string) bool {
	_, followed := f.mapPollingList.Load(stackId)
	f.Log.Info("Following Stack", "StackID", stackId, "Following", followed)
	return followed
}

// Identify if the follower is actively working this one.
func (f *StackFollower) startFollowing(stack *v1alpha1.Stack) {
	namespacedName := &types.NamespacedName{Name: stack.Name, Namespace: stack.Namespace}
	f.mapPollingList.Store(stack.Status.StackID, namespacedName)
	f.Log.Info("Now following Stack", "StackID", stack.Status.StackID)
	f.StacksFollowed.Inc()
	f.StacksFollowing.Inc()
}

// Identify if the follower is actively working this one.
func (f *StackFollower) stopFollowing(stackId string) {
	f.mapPollingList.Delete(stackId)
	f.Log.Info("Stopped following Stack", "StackID", stackId)
	f.StacksFollowing.Dec()
}

// Allow passing a current/recent fetch of the stack object to the method (optionally)
func (f *StackFollower) updateStackStatus(ctx context.Context, instance *v1alpha1.Stack, stack ...*cfTypes.Stack) error {
	var err error
	var cfs *cfTypes.Stack
	update := false
	log := f.Log.WithValues("StackID", instance.Status.StackID, "UID", instance.UID, "Namespace",
		instance.Namespace, "Name", instance.Name)

	if len(stack) > 0 {
		cfs = stack[0]
	}
	if cfs == nil {
		cfs, err = f.CloudFormationHelper.GetStack(ctx, instance)
		if err != nil {
			log.Error(err, "Failed to get CloudFormation stack")
			return err
		}
	}

	outputs := map[string]string{}
	if cfs.Outputs != nil && len(cfs.Outputs) > 0 {
		for _, output := range cfs.Outputs {
			outputs[*output.OutputKey] = *output.OutputValue
		}
	}

	// Checking the status
	if string(cfs.StackStatus) != instance.Status.StackStatus {
		update = true
		instance.Status.StackStatus = string(cfs.StackStatus)

		createdTime := metav1.NewTime(*cfs.CreationTime)
		instance.Status.CreatedTime = &createdTime

		if cfs.LastUpdatedTime != nil {
			updatedTime := metav1.NewTime(*cfs.LastUpdatedTime)
			instance.Status.UpdatedTime = &updatedTime
		}
	}

	// Checking stack ID and outputs for changes.
	stackID := *cfs.StackId
	if stackID != instance.Status.StackID || !reflect.DeepEqual(outputs, instance.Status.Outputs) {
		update = true
		instance.Status.StackID = stackID
		if len(outputs) > 0 {
			instance.Status.Outputs = outputs
		}
	}

	// Recording the Role ARN
	roleArn := cfs.RoleARN
	if roleArn != nil && *roleArn != "" {
		instance.Status.RoleARN = *roleArn
	} else {
		instance.Status.RoleARN = ""
	}

	// Recording all stack resources
	resources, err := f.CloudFormationHelper.GetStackResources(ctx, instance.Status.StackID)
	if err != nil {
		log.Error(err, "Failed to get Stack Resources")
		return err
	}
	if !reflect.DeepEqual(resources, instance.Status.Resources) {
		update = true
		instance.Status.Resources = resources
	}

	if update {
		err = f.Status().Update(ctx, instance)
		if err != nil {
			log.Error(err, "Failed to update Stack Status")
			if errors.IsNotFound(err) {
				// Request object not found, could have been deleted after reconcile request.
				// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
				// Return and don't requeue
				// return reconcile.Result{}, nil
				return nil
			}
			// Error reading the object - requeue the request.
			// return reconcile.Result{}, err
			return err
		}
	}

	return nil
}

func (f *StackFollower) processStack(key interface{}, value interface{}) bool {

	stackId := key.(string)
	namespacedName := value.(*types.NamespacedName)
	stack := &v1alpha1.Stack{}
	log := f.Log.WithValues("StackID", stackId, "Namespace",
		namespacedName.Namespace, "Name", namespacedName.Name)

	// Fetch the Stack instance
	err := f.Client.Get(context.TODO(), *namespacedName, stack)
	if err != nil {
		if errors.IsNotFound(err) {
			f.Log.Info("Stack resource not found. Ignoring since object must be deleted")
			f.stopFollowing(stackId)
			return true
		}
		// Error reading the object - requeue the request.
		f.Log.Error(err, "Failed to get Stack on this pass, requeuing")
		return true
	}
	log = log.WithValues("UID", stack.UID)

	cfs, err := f.CloudFormationHelper.GetStack(context.TODO(), stack)
	if err != nil {
		if err == ErrStackNotFound {
			log.Error(err, "Stack Not Found")
			f.stopFollowing(stackId)
		} else {
			log.Error(err, "Error retrieving stack for processing")
		}
	} else {
		err = f.updateStackStatus(context.TODO(), stack, cfs)
		if err != nil {
			log.Error(err, "Failed to update stack status")
		} else if f.CloudFormationHelper.StackInTerminalState(cfs.StackStatus) {
			f.stopFollowing(stackId)
		}
	}

	return true
}

func (f *StackFollower) Worker() {
	for {
		time.Sleep(time.Second)
		f.mapPollingList.Range(f.processStack)
	}
}
