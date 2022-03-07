package scc

import (
	"fmt"
	"net/http"
	"strings"

	securityv1 "github.com/openshift/api/security/v1"
	"github.com/openshift/managed-cluster-validating-webhooks/pkg/webhooks/utils"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	admissionctl "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	WebhookName string = "scc-validation"
	docString   string = `Managed OpenShift Customers may not modify the following default SCCs: %s`
)

var (
	timeout int32 = 2
	log           = logf.Log.WithName(WebhookName)
	scope         = admissionregv1.AllScopes
	rules         = []admissionregv1.RuleWithOperations{
		{
			Operations: []admissionregv1.OperationType{"UPDATE", "DELETE"},
			Rule: admissionregv1.Rule{
				APIGroups:   []string{"security.openshift.io"},
				APIVersions: []string{"*"},
				Resources:   []string{"securitycontextconstraints"},
				Scope:       &scope,
			},
		},
		{
			Operations: []admissionregv1.OperationType{"UPDATE"},
			Rule: admissionregv1.Rule{
				APIGroups:   []string{"rbac.authorization.k8s.io"},
				APIVersions: []string{"*"},
				Resources:   []string{"clusterrolebindings"},
				Scope:       &scope,
			},
		},
	}
	allowedUsers = []string{
		"system:serviceaccount:openshift-monitoring:cluster-monitoring-operator",
	}
	allowedGroups = []string{}
	defaultSCCs   = []string{
		"anyuid",
		"hostaccess",
		"hostmount-anyuid",
		"hostnetwork",
		"node-exporter",
		"nonroot",
		"privileged",
		"restricted",
		"pipelines-scc",
	}
	defaultClusterRoles = []string{
		"system:openshift:scc:anyuid",
		"system:openshift:scc:hostaccess",
		"system:openshift:scc:hostmount-anyuid",
		"system:openshift:scc:hostnetwork",
		"system:openshift:scc:node-exporter",
		"system:openshift:scc:nonroot",
		"system:openshift:scc:privileged",
		"system:openshift:scc:restricted",
		"system:openshift:scc:pipelines-scc",
	}
	forbiddenCRBSubjects = []string{
		"system:authenticated",
	}
)

type SCCWebHook struct {
	s runtime.Scheme
}

// NewWebhook creates the new webhook
func NewWebhook() *SCCWebHook {
	scheme := runtime.NewScheme()
	admissionv1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	return &SCCWebHook{
		s: *scheme,
	}
}

// Authorized implements Webhook interface
func (s *SCCWebHook) Authorized(request admissionctl.Request) admissionctl.Response {
	return s.authorized(request)
}

func (s *SCCWebHook) authorized(request admissionctl.Request) admissionctl.Response {
	var ret admissionctl.Response

	scc, err := s.renderSCC(request)
	if err != nil {
		log.Error(err, "Couldn't render a SCC from the incoming request")
		return admissionctl.Errored(http.StatusBadRequest, err)
	}

	crb, err := s.renderCRB(request)
	if err != nil {
		log.Error(err, "Couldn't render a ClusterRoleBinding from the incoming request")
		return admissionctl.Errored(http.StatusBadRequest, err)
	}

	if isDefaultClusterRole(crb) && isForbiddenCRBSubject(crb) && request.Operation == admissionv1.Update {
		log.Info(fmt.Sprintf("Attempt to add forbidden group detected on ClusterRoleBinding: %v", crb.Name))
		ret = admissionctl.Denied(fmt.Sprintf("Adding group: %v to the default SCC: %v is not allowed", forbiddenCRBSubjects, crb.RoleRef.Name[strings.LastIndex(crb.RoleRef.Name, ":")+1:]))
		ret.UID = request.AdmissionRequest.UID
		return ret
	}

	if isDefaultSCC(scc) && !isAllowedUserGroup(request) {
		switch request.Operation {
		case admissionv1.Delete:
			log.Info(fmt.Sprintf("Deleting operation detected on default SCC: %v", scc.Name))
			ret = admissionctl.Denied(fmt.Sprintf("Deleting default SCCs %v is not allowed", defaultSCCs))
			ret.UID = request.AdmissionRequest.UID
			return ret
		case admissionv1.Update:
			log.Info(fmt.Sprintf("Updating operation detected on default SCC: %v", scc.Name))
			ret = admissionctl.Denied(fmt.Sprintf("Modifying default SCCs %v is not allowed", defaultSCCs))
			ret.UID = request.AdmissionRequest.UID
			return ret
		}
	}

	ret = admissionctl.Allowed("Request is allowed")
	ret.UID = request.AdmissionRequest.UID
	return ret
}

// renderSCC render the SCC object from the requests
func (s *SCCWebHook) renderSCC(request admissionctl.Request) (*securityv1.SecurityContextConstraints, error) {
	decoder, err := admissionctl.NewDecoder(&s.s)
	if err != nil {
		return nil, err
	}
	scc := &securityv1.SecurityContextConstraints{}

	if len(request.OldObject.Raw) > 0 {
		err = decoder.DecodeRaw(request.OldObject, scc)
	}
	if err != nil {
		return nil, err
	}

	return scc, nil
}

// renderCRB render the ClusterRoleBinding object from the requests
func (s *SCCWebHook) renderCRB(request admissionctl.Request) (*rbacv1.ClusterRoleBinding, error) {
	decoder, err := admissionctl.NewDecoder(&s.s)
	if err != nil {
		return nil, err
	}
	crb := &rbacv1.ClusterRoleBinding{}

	if len(request.OldObject.Raw) > 0 {
		err = decoder.DecodeRaw(request.OldObject, crb)
	}
	if err != nil {
		return nil, err
	}

	return crb, nil
}

// isAllowedUserGroup checks if the user or group is allowed to perform the action
func isAllowedUserGroup(request admissionctl.Request) bool {
	if utils.SliceContains(request.UserInfo.Username, allowedUsers) {
		return true
	}

	for _, group := range allowedGroups {
		if utils.SliceContains(group, request.UserInfo.Groups) {
			return true
		}
	}

	return false
}

func isForbiddenCRBSubject(crb *rbacv1.ClusterRoleBinding) bool {
	for _, subject := range crb.Subjects {
		for _, group := range forbiddenCRBSubjects {
			if subject.Name == group {
				return true
			}
		}
	}
	return false
}

// isDefaultSCC checks if the request is going to operate on the SCC in the
// default list
func isDefaultSCC(scc *securityv1.SecurityContextConstraints) bool {
	for _, s := range defaultSCCs {
		if scc.Name == s {
			return true
		}
	}
	return false
}

func isDefaultClusterRole(crb *rbacv1.ClusterRoleBinding) bool {
	for _, d := range defaultClusterRoles {
		if crb.RoleRef.Name == d {
			return true
		}
	}
	return false
}

// GetURI implements Webhook interface
func (s *SCCWebHook) GetURI() string {
	return "/" + WebhookName
}

// Validate implements Webhook interface
func (s *SCCWebHook) Validate(request admissionctl.Request) bool {
	valid := true
	valid = valid && (request.UserInfo.Username != "")
	valid = valid && (request.Kind.Kind == "SecurityContextConstraints")

	return valid
}

// Name implements Webhook interface
func (s *SCCWebHook) Name() string {
	return WebhookName
}

// FailurePolicy implements Webhook interface
func (s *SCCWebHook) FailurePolicy() admissionregv1.FailurePolicyType {
	return admissionregv1.Ignore
}

// MatchPolicy implements Webhook interface
func (s *SCCWebHook) MatchPolicy() admissionregv1.MatchPolicyType {
	return admissionregv1.Equivalent
}

// Rules implements Webhook interface
func (s *SCCWebHook) Rules() []admissionregv1.RuleWithOperations {
	return rules
}

// ObjectSelector implements Webhook interface
func (s *SCCWebHook) ObjectSelector() *metav1.LabelSelector {
	return nil
}

// SideEffects implements Webhook interface
func (s *SCCWebHook) SideEffects() admissionregv1.SideEffectClass {
	return admissionregv1.SideEffectClassNone
}

// TimeoutSeconds implements Webhook interface
func (s *SCCWebHook) TimeoutSeconds() int32 {
	return timeout
}

// Doc implements Webhook interface
func (s *SCCWebHook) Doc() string {
	return fmt.Sprintf(docString, defaultSCCs)
}

// SyncSetLabelSelector returns the label selector to use in the SyncSet.
// Return utils.DefaultLabelSelector() to stick with the default
func (s *SCCWebHook) SyncSetLabelSelector() metav1.LabelSelector {
	return utils.DefaultLabelSelector()
}
