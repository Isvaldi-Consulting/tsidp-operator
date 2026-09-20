package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tsidpv1alpha1 "github.com/isvaldi-consulting/tsidp-operator/api/v1alpha1"
	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp"
	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp/tsidpfake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := tsidpv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newCR(name string) *tsidpv1alpha1.OIDCClient {
	return &tsidpv1alpha1.OIDCClient{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("aabbccdd-0000-1111-2222-333344445555"),
		},
		Spec: tsidpv1alpha1.OIDCClientSpec{
			RedirectURIs: []string{"https://app.example.ts.net/callback"},
		},
	}
}

type harness struct {
	r   *OIDCClientReconciler
	k8s client.Client
	idp *tsidpfake.Server
	req ctrl.Request
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	fakeIdp, ts := tsidpfake.New()
	t.Cleanup(ts.Close)
	s := newScheme(t)
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&tsidpv1alpha1.OIDCClient{}).
		WithObjects(objs...).
		Build()
	return &harness{
		r: &OIDCClientReconciler{
			Client:         k8s,
			APIReader:      k8s,
			Scheme:         s,
			Recorder:       record.NewFakeRecorder(100),
			Tsidp:          &tsidp.Client{BaseURL: ts.URL, HTTPClient: ts.Client()},
			ResyncInterval: 10 * time.Minute,
		},
		k8s: k8s,
		idp: fakeIdp,
		req: ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}},
	}
}

// settle reconciles until the controller stops asking for a near-immediate
// requeue (internal state machine steps use RequeueAfter <= 1s).
func (h *harness) settle(t *testing.T) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	var err error
	for i := 0; i < 10; i++ {
		res, err = h.r.Reconcile(context.Background(), h.req)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter > time.Second {
			return res
		}
	}
	return res
}

func (h *harness) getCR(t *testing.T) *tsidpv1alpha1.OIDCClient {
	t.Helper()
	var oc tsidpv1alpha1.OIDCClient
	if err := h.k8s.Get(context.Background(), h.req.NamespacedName, &oc); err != nil {
		t.Fatalf("get CR: %v", err)
	}
	return &oc
}

func (h *harness) getSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	if err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &sec); err != nil {
		t.Fatalf("get Secret %s: %v", name, err)
	}
	return &sec
}

func TestCreateRegistersAndWritesSecret(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID == "" {
		t.Fatal("status.clientId not set")
	}
	if !meta.IsStatusConditionTrue(oc.Status.Conditions, tsidpv1alpha1.ConditionReady) {
		t.Fatalf("Ready condition not true: %+v", oc.Status.Conditions)
	}

	sec := h.getSecret(t, "app-oidc")
	for _, k := range []string{"client_id", "client_secret", "issuer", "authorization_endpoint", "token_endpoint", "userinfo_endpoint", "jwks_uri", "discovery_endpoint", "redirect_uris"} {
		if len(sec.Data[k]) == 0 {
			t.Fatalf("Secret missing key %q", k)
		}
	}
	if string(sec.Data["client_id"]) != oc.Status.ClientID {
		t.Fatal("Secret client_id does not match status")
	}
	ref := metav1.GetControllerOf(sec)
	if ref == nil || ref.Kind != "OIDCClient" {
		t.Fatalf("Secret has no OIDCClient controller ownerReference: %+v", sec.OwnerReferences)
	}

	remote := h.idp.Snapshot()
	rc, ok := remote[oc.Status.ClientID]
	if !ok {
		t.Fatal("client not present in tsidp")
	}
	if !strings.Contains(rc.ClientName, Fingerprint(oc)) {
		t.Fatalf("registered name %q missing fingerprint", rc.ClientName)
	}
}

func TestDriftRecreateIssuesNewCredentials(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)
	firstID := oc.Status.ClientID
	firstSecret := string(h.getSecret(t, "app-oidc").Data["client_secret"])

	oc.Spec.RedirectURIs = []string{"https://app.example.ts.net/new-callback"}
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	oc = h.getCR(t)
	if oc.Status.ClientID == firstID {
		t.Fatal("Recreate policy should have issued a new client_id")
	}
	sec := h.getSecret(t, "app-oidc")
	if string(sec.Data["client_secret"]) == firstSecret {
		t.Fatal("Secret should hold new credentials")
	}
	if string(sec.Data["redirect_uris"]) != "https://app.example.ts.net/new-callback" {
		t.Fatalf("Secret redirect_uris stale: %q", sec.Data["redirect_uris"])
	}
	if _, stale := h.idp.Snapshot()[firstID]; stale {
		t.Fatal("old client not deleted from tsidp")
	}
}

func TestDriftInPlacePreservesCredentials(t *testing.T) {
	cr := newCR("app")
	cr.Spec.UpdatePolicy = tsidpv1alpha1.UpdatePolicyInPlace
	h := newHarness(t, cr)
	h.settle(t)
	oc := h.getCR(t)
	firstID := oc.Status.ClientID

	oc.Spec.RedirectURIs = append(oc.Spec.RedirectURIs, "https://second.example.ts.net/cb")
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	oc = h.getCR(t)
	if oc.Status.ClientID != firstID {
		t.Fatal("InPlace policy must preserve client_id")
	}
	if got := h.idp.Snapshot()[firstID]; len(got.RedirectURIs) != 2 {
		t.Fatalf("remote redirect URIs not updated: %+v", got.RedirectURIs)
	}
}

func TestDriftBlockReportsNotReady(t *testing.T) {
	cr := newCR("app")
	cr.Spec.UpdatePolicy = tsidpv1alpha1.UpdatePolicyBlock
	h := newHarness(t, cr)
	h.settle(t)
	oc := h.getCR(t)
	firstID := oc.Status.ClientID

	oc.Spec.RedirectURIs = []string{"https://changed.example.ts.net/cb"}
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	oc = h.getCR(t)
	if oc.Status.ClientID != firstID {
		t.Fatal("Block policy must not touch the registration")
	}
	if meta.IsStatusConditionTrue(oc.Status.Conditions, tsidpv1alpha1.ConditionReady) {
		t.Fatalf("expected Ready=False: %+v", oc.Status.Conditions)
	}
	if c := meta.FindStatusCondition(oc.Status.Conditions, tsidpv1alpha1.ConditionReady); c.Reason != "DriftBlocked" {
		t.Fatalf("expected reason DriftBlocked, got %q", c.Reason)
	}
}

func TestSecretDeletionRotatesCredentials(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	firstID := h.getCR(t).Status.ClientID

	if err := h.k8s.Delete(context.Background(), h.getSecret(t, "app-oidc")); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID == firstID {
		t.Fatal("lost Secret must force re-registration (plaintext secret is unrecoverable)")
	}
	sec := h.getSecret(t, "app-oidc")
	if len(sec.Data["client_secret"]) == 0 {
		t.Fatal("Secret not rewritten")
	}
	if _, stale := h.idp.Snapshot()[firstID]; stale {
		t.Fatal("stale registration not cleaned up")
	}
}

func TestRegistrationLostReRegisters(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	firstID := h.getCR(t).Status.ClientID

	// Simulate tsidp losing its state (restart without the PVC).
	for id := range h.idp.Snapshot() {
		_ = h.r.Tsidp.DeleteClient(context.Background(), id)
	}
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID == "" || oc.Status.ClientID == firstID {
		t.Fatalf("expected fresh registration, got %q", oc.Status.ClientID)
	}
	if string(h.getSecret(t, "app-oidc").Data["client_id"]) != oc.Status.ClientID {
		t.Fatal("Secret not updated after re-registration")
	}
}

func TestAdoptionWithIntactSecret(t *testing.T) {
	cr := newCR("app")
	fp := Fingerprint(cr)
	ctrlRef := true
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-oidc",
			Namespace: "default",
			Labels:    ManagedSecretLabels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "tsidp.isvaldi-consulting.github.io/v1alpha1",
				Kind:               "OIDCClient",
				Name:               cr.Name,
				UID:                cr.UID,
				Controller:         &ctrlRef,
				BlockOwnerDeletion: &ctrlRef,
			}},
		},
		Data: map[string][]byte{
			"client_id":     []byte("pre-existing-id"),
			"client_secret": []byte("pre-existing-secret"),
		},
	}
	h := newHarness(t, cr, sec)
	h.idp.Seed(tsidpfake.Client{
		ClientID:     "pre-existing-id",
		ClientName:   "app " + fp,
		RedirectURIs: cr.Spec.RedirectURIs,
	})
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID != "pre-existing-id" {
		t.Fatalf("expected adoption of pre-existing-id, got %q", oc.Status.ClientID)
	}
	if string(h.getSecret(t, "app-oidc").Data["client_secret"]) != "pre-existing-secret" {
		t.Fatal("adoption must not rewrite credentials")
	}
}

func TestAdoptionWithoutSecretBurnsOrphan(t *testing.T) {
	cr := newCR("app")
	h := newHarness(t, cr)
	h.idp.Seed(tsidpfake.Client{
		ClientID:     "orphan-id",
		ClientName:   "app " + Fingerprint(cr),
		RedirectURIs: cr.Spec.RedirectURIs,
	})
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID == "" || oc.Status.ClientID == "orphan-id" {
		t.Fatalf("expected orphan burned and fresh registration, got %q", oc.Status.ClientID)
	}
	if _, stale := h.idp.Snapshot()["orphan-id"]; stale {
		t.Fatal("unadoptable orphan not deleted")
	}
}

func TestDeleteDeregistersAndRemovesFinalizer(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)
	id := oc.Status.ClientID

	if err := h.k8s.Delete(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	// Object persists under the finalizer with a deletionTimestamp.
	if _, err := h.r.Reconcile(context.Background(), h.req); err != nil {
		t.Fatalf("Reconcile(delete): %v", err)
	}

	if _, still := h.idp.Snapshot()[id]; still {
		t.Fatal("client not deregistered from tsidp")
	}
	var gone tsidpv1alpha1.OIDCClient
	err := h.k8s.Get(context.Background(), h.req.NamespacedName, &gone)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("CR should be gone after finalizer removal, got %v", err)
	}
}

func TestDeleteOrphanPolicyKeepsRegistration(t *testing.T) {
	cr := newCR("app")
	cr.Spec.DeletionPolicy = tsidpv1alpha1.DeletionPolicyOrphan
	h := newHarness(t, cr)
	h.settle(t)
	id := h.getCR(t).Status.ClientID

	if err := h.k8s.Delete(context.Background(), h.getCR(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(context.Background(), h.req); err != nil {
		t.Fatal(err)
	}
	if _, still := h.idp.Snapshot()[id]; !still {
		t.Fatal("Orphan policy must leave the tsidp registration in place")
	}
}

func TestExistingUnownedSecretBlocksRegistration(t *testing.T) {
	userSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-oidc", Namespace: "default"},
		Data:       map[string][]byte{"unrelated": []byte("user data")},
	}
	h := newHarness(t, newCR("app"), userSecret)
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID != "" {
		t.Fatal("must not register while the target Secret is not ours")
	}
	if len(h.idp.Snapshot()) != 0 {
		t.Fatal("must not create remote state while the target Secret is not ours")
	}
	if c := meta.FindStatusCondition(oc.Status.Conditions, tsidpv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SecretConflict" {
		t.Fatalf("expected Ready=False reason SecretConflict: %+v", oc.Status.Conditions)
	}
	sec := h.getSecret(t, "app-oidc")
	if string(sec.Data["unrelated"]) != "user data" {
		t.Fatal("user Secret content must not be touched")
	}
}

func TestInPlaceUpdateRejectionReportsNotReady(t *testing.T) {
	cr := newCR("app")
	cr.Spec.UpdatePolicy = tsidpv1alpha1.UpdatePolicyInPlace
	h := newHarness(t, cr)
	h.settle(t)
	oc := h.getCR(t)
	firstID := oc.Status.ClientID

	// tsidp's /register accepts mailto: but its /edit validation rejects
	// it — at HTTP 200. The verify-after-write in UpdateClient must catch
	// the silent rejection and report Ready=False instead of success.
	oc.Spec.RedirectURIs = []string{"mailto:ops@example.com"}
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	oc = h.getCR(t)
	if c := meta.FindStatusCondition(oc.Status.Conditions, tsidpv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InPlaceUpdateFailed" {
		t.Fatalf("expected Ready=False with reason InPlaceUpdateFailed: %+v", oc.Status.Conditions)
	}
	if oc.Status.ClientID != firstID {
		t.Fatal("failed in-place update must not rotate credentials")
	}
}

func TestDeleteCleansUpUnrecordedRegistration(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)
	id := oc.Status.ClientID

	// Simulate a crash after POST /register but before the status write:
	// the registration exists and is fingerprinted, but status is empty.
	oc.Status.ClientID = ""
	if err := h.k8s.Status().Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if err := h.k8s.Delete(context.Background(), h.getCR(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(context.Background(), h.req); err != nil {
		t.Fatal(err)
	}

	if _, still := h.idp.Snapshot()[id]; still {
		t.Fatal("finalizer must clean up fingerprinted registrations even when status.clientId was never persisted")
	}
}

func TestSecretTemplateKeyRemovalConverges(t *testing.T) {
	cr := newCR("app")
	cr.Spec.SecretTemplate = &tsidpv1alpha1.SecretTemplate{
		Labels:      map[string]string{"team": "platform"},
		Annotations: map[string]string{"reloader.stakater.com/match": "true"},
	}
	h := newHarness(t, cr)
	h.settle(t)
	sec := h.getSecret(t, "app-oidc")
	if sec.Labels["team"] != "platform" || sec.Annotations["reloader.stakater.com/match"] != "true" {
		t.Fatalf("template metadata not applied: labels=%v annotations=%v", sec.Labels, sec.Annotations)
	}

	oc := h.getCR(t)
	oc.Spec.SecretTemplate = &tsidpv1alpha1.SecretTemplate{
		Labels: map[string]string{"squad": "idp"},
	}
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	sec = h.getSecret(t, "app-oidc")
	if _, stale := sec.Labels["team"]; stale {
		t.Fatal("label removed from secretTemplate must be removed from the Secret")
	}
	if _, stale := sec.Annotations["reloader.stakater.com/match"]; stale {
		t.Fatal("annotation removed from secretTemplate must be removed from the Secret")
	}
	if sec.Labels["squad"] != "idp" {
		t.Fatal("new template label not applied")
	}
	if sec.Labels["app.kubernetes.io/managed-by"] != "tsidp-operator" {
		t.Fatal("managed-by label must survive template changes")
	}
}

func TestSecretRenameRotatesCredentials(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)
	firstID := oc.Status.ClientID

	oc.Spec.SecretName = "renamed-oidc"
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	// Rename deliberately reuses the SecretLost -> re-register flow:
	// new credentials at the new name, old Secret removed.
	oc = h.getCR(t)
	if oc.Status.ClientID == firstID || oc.Status.ClientID == "" {
		t.Fatalf("rename should re-register with new credentials, got %q", oc.Status.ClientID)
	}
	sec := h.getSecret(t, "renamed-oidc")
	if string(sec.Data["client_id"]) != oc.Status.ClientID || len(sec.Data["client_secret"]) == 0 {
		t.Fatal("renamed Secret must hold the new credentials")
	}
	var gone corev1.Secret
	err := h.k8s.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app-oidc"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("old Secret should be deleted after rename, got %v", err)
	}
	if _, stale := h.idp.Snapshot()[firstID]; stale {
		t.Fatal("old registration should be burned after rename")
	}
}

func TestRenameOntoUnownedSecretIsBlocked(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)
	id := oc.Status.ClientID
	origSecretData := string(h.getSecret(t, "app-oidc").Data["client_secret"])

	squatter := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "taken-oidc", Namespace: "default"},
		Data:       map[string][]byte{"theirs": []byte("do not touch")},
	}
	if err := h.k8s.Create(context.Background(), squatter); err != nil {
		t.Fatal(err)
	}
	oc.Spec.SecretName = "taken-oidc"
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	// The rename must be blocked BEFORE anything is destroyed: old Secret
	// and registration intact, squatter untouched, Ready=False.
	oc = h.getCR(t)
	if oc.Status.ClientID != id {
		t.Fatal("blocked rename must not burn the registration")
	}
	if got := string(h.getSecret(t, "app-oidc").Data["client_secret"]); got != origSecretData {
		t.Fatal("blocked rename must not touch the old credentials Secret")
	}
	if got := string(h.getSecret(t, "taken-oidc").Data["theirs"]); got != "do not touch" {
		t.Fatal("blocked rename must not touch the squatting Secret")
	}
	if c := meta.FindStatusCondition(oc.Status.Conditions, tsidpv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SecretConflict" {
		t.Fatalf("expected Ready=False reason SecretConflict: %+v", oc.Status.Conditions)
	}
	if _, still := h.idp.Snapshot()[id]; !still {
		t.Fatal("registration must survive a blocked rename")
	}
}

func TestRenameLeavesForeignSecretAtOldName(t *testing.T) {
	h := newHarness(t, newCR("app"))
	h.settle(t)
	oc := h.getCR(t)

	// Someone deletes our Secret and recreates an unrelated one at the old
	// name, then the CR is renamed: the rename cleanup must not delete the
	// foreign Secret.
	if err := h.k8s.Delete(context.Background(), h.getSecret(t, "app-oidc")); err != nil {
		t.Fatal(err)
	}
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-oidc", Namespace: "default"},
		Data:       map[string][]byte{"theirs": []byte("keep")},
	}
	if err := h.k8s.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	oc.Spec.SecretName = "renamed-oidc"
	if err := h.k8s.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	h.settle(t)

	if got := string(h.getSecret(t, "app-oidc").Data["theirs"]); got != "keep" {
		t.Fatal("rename cleanup must not delete a Secret it no longer owns")
	}
	oc = h.getCR(t)
	if oc.Status.ClientID == "" || string(h.getSecret(t, "renamed-oidc").Data["client_id"]) != oc.Status.ClientID {
		t.Fatal("rename should still converge at the new name")
	}
}

func TestWhitespaceRedirectURIRejected(t *testing.T) {
	cr := newCR("app")
	cr.Spec.RedirectURIs = []string{"https://a.example/cb https://evil.example/cb"}
	h0 := newHarness(t, cr)
	h0.settle(t)
	if oc := h0.getCR(t); oc.Status.ClientID != "" {
		t.Fatal("space-bearing URI must never reach tsidp")
	}

	// A leading control character can mask a dangerous scheme past
	// prefix-matching validators (e.g. "\vjavascript:") — the guard must
	// reject control runes, not just ASCII whitespace.
	cr = newCR("app")
	cr.Spec.RedirectURIs = []string{"\vjavascript:alert(1)"}
	h1 := newHarness(t, cr)
	h1.settle(t)
	if oc := h1.getCR(t); oc.Status.ClientID != "" {
		t.Fatal("control-rune-masked scheme must never reach tsidp")
	}

	// \x01 is IsControl but NOT IsSpace — pins the IsControl clause
	// independently of the whitespace check.
	cr = newCR("app")
	cr.Spec.RedirectURIs = []string{"\x01https://a.example/cb"}
	h := newHarness(t, cr)
	h.settle(t)

	oc := h.getCR(t)
	if oc.Status.ClientID != "" || len(h.idp.Snapshot()) != 0 {
		t.Fatal("a whitespace-bearing redirect URI must never reach tsidp")
	}
	if c := meta.FindStatusCondition(oc.Status.Conditions, tsidpv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "SpecInvalid" {
		t.Fatalf("expected Ready=False reason SpecInvalid: %+v", oc.Status.Conditions)
	}
}
