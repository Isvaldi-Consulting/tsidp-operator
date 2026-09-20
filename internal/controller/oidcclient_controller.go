// Package controller reconciles OIDCClient objects against a tsidp instance.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	tsidpv1alpha1 "github.com/isvaldi-consulting/tsidp-operator/api/v1alpha1"
	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp"
)

const (
	// FinalizerName guards deregistration of the tsidp client.
	FinalizerName = "oidcclient.tsidp.isvaldi-consulting.github.io/deregister"

	// ForceRemoveFinalizerAnnotation, when present on a deleted OIDCClient,
	// skips remote deregistration so a permanently unreachable tsidp cannot
	// wedge deletion.
	ForceRemoveFinalizerAnnotation = "tsidp.isvaldi-consulting.github.io/force-remove-finalizer"

	// templateKeysAnnotation records which label/annotation keys were
	// applied from spec.secretTemplate, so keys removed from the template
	// are removed from the Secret instead of lingering forever.
	templateKeysAnnotation = "tsidp.isvaldi-consulting.github.io/template-keys"

	errBackoff = 30 * time.Second
)

// errSecretConflict marks failures caused by a Secret name collision with
// an object this CR does not own — surfaced with its own condition reason
// so a pre-squatted name reads as a conflict, not a generic failure.
var errSecretConflict = errors.New("secret name conflict")

// ManagedSecretLabels is stamped on every credentials Secret. main.go scopes
// the manager's Secret informer to this label so the operator does not cache
// (or need RBAC-watch) every Secret in the cluster.
var ManagedSecretLabels = map[string]string{
	"app.kubernetes.io/managed-by": "tsidp-operator",
}

// OIDCClientReconciler registers OIDCClient objects with tsidp and
// materializes the credentials into Secrets.
type OIDCClientReconciler struct {
	client.Client
	// APIReader reads directly from the API server, bypassing the informer
	// cache. Every read that gates a DESTRUCTIVE decision (burning a
	// registration because its Secret looks wrong) must use it: the cached
	// client can serve reads that are one write behind our own writes.
	APIReader      client.Reader
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	Tsidp          *tsidp.Client
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=tsidp.isvaldi-consulting.github.io,resources=oidcclients,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=tsidp.isvaldi-consulting.github.io,resources=oidcclients/status,verbs=get;update
// +kubebuilder:rbac:groups=tsidp.isvaldi-consulting.github.io,resources=oidcclients/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Fingerprint returns the ownership marker the operator appends to the
// client_name registered with tsidp. tsidp's client object has no free-form
// metadata field, so the name is the only place identity can live. The CRD
// forbids "[k8s:" in spec.clientName so user input can never shadow it.
func Fingerprint(oc *tsidpv1alpha1.OIDCClient) string {
	return fmt.Sprintf("[k8s:%s/%s/%.8s]", oc.Namespace, oc.Name, string(oc.UID))
}

func displayName(oc *tsidpv1alpha1.OIDCClient) string {
	name := strings.TrimSpace(oc.Spec.ClientName)
	if name == "" {
		name = oc.Name
	}
	return name
}

func registeredName(oc *tsidpv1alpha1.OIDCClient) string {
	return displayName(oc) + " " + Fingerprint(oc)
}

func secretName(oc *tsidpv1alpha1.OIDCClient) string {
	if oc.Spec.SecretName != "" {
		return oc.Spec.SecretName
	}
	return oc.Name + "-oidc"
}

// registerOutcome reports how registerOrAdopt established (or failed to
// establish) status.ClientID.
type registerOutcome int

const (
	outcomeInvalid registerOutcome = iota // zero value; only ever paired with a non-nil error
	outcomeAdopted
	outcomeRegistered
	outcomeBurnedOrphan
)

// Reconcile drives one OIDCClient toward its registered state.
func (r *OIDCClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var oc tsidpv1alpha1.OIDCClient
	if err := r.Get(ctx, req.NamespacedName, &oc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !oc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &oc)
	}

	if !controllerutil.ContainsFinalizer(&oc, FinalizerName) {
		controllerutil.AddFinalizer(&oc, FinalizerName)
		if err := r.Update(ctx, &oc); err != nil {
			return ctrl.Result{}, err
		}
	}

	res, reconcileErr := r.reconcileNormal(ctx, &oc)

	oc.Status.ObservedGeneration = oc.Generation
	if err := r.Status().Update(ctx, &oc); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		log.Error(err, "updating status")
		return ctrl.Result{}, err
	}
	return res, reconcileErr
}

func (r *OIDCClientReconciler) reconcileDelete(ctx context.Context, oc *tsidpv1alpha1.OIDCClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(oc, FinalizerName) {
		return ctrl.Result{}, nil
	}
	_, force := oc.Annotations[ForceRemoveFinalizerAnnotation]
	switch {
	case force:
		// Escape hatch: skip all remote calls.
	case oc.Spec.DeletionPolicy == tsidpv1alpha1.DeletionPolicyOrphan:
		// The registration stays behind (fingerprint and all — harmless,
		// nothing collects it; there is deliberately no GC sweeper).
	default: // Deregister
		if oc.Status.ClientID != "" {
			if err := r.Tsidp.DeleteClient(ctx, oc.Status.ClientID); err != nil && !errors.Is(err, tsidp.ErrNotFound) {
				r.Recorder.Eventf(oc, corev1.EventTypeWarning, "DeregisterFailed",
					"failed to deregister client %s from tsidp (will retry; annotate with %s to skip): %v",
					oc.Status.ClientID, ForceRemoveFinalizerAnnotation, err)
				return ctrl.Result{RequeueAfter: errBackoff}, nil
			}
			r.Recorder.Eventf(oc, corev1.EventTypeNormal, "Deregistered",
				"deregistered client %s from tsidp (outstanding tokens revoked)", oc.Status.ClientID)
		} else {
			// A registration may exist that status never recorded (crash
			// or persistent Secret-write failure after POST /register).
			// Find it by fingerprint so deletion does not leak it.
			list, err := r.Tsidp.ListClients(ctx)
			if err != nil {
				r.Recorder.Eventf(oc, corev1.EventTypeWarning, "DeregisterFailed",
					"failed to list tsidp clients for fingerprint cleanup (will retry; annotate with %s to skip): %v",
					ForceRemoveFinalizerAnnotation, err)
				return ctrl.Result{RequeueAfter: errBackoff}, nil
			}
			fp := Fingerprint(oc)
			for i := range list {
				if !strings.Contains(list[i].ClientName, fp) {
					continue
				}
				if err := r.Tsidp.DeleteClient(ctx, list[i].ClientID); err != nil && !errors.Is(err, tsidp.ErrNotFound) {
					r.Recorder.Eventf(oc, corev1.EventTypeWarning, "DeregisterFailed",
						"failed to delete fingerprinted client %s (will retry): %v", list[i].ClientID, err)
					return ctrl.Result{RequeueAfter: errBackoff}, nil
				}
				r.Recorder.Eventf(oc, corev1.EventTypeNormal, "Deregistered",
					"deleted unrecorded fingerprinted client %s during finalization", list[i].ClientID)
			}
		}
	}
	controllerutil.RemoveFinalizer(oc, FinalizerName)
	if err := r.Update(ctx, oc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *OIDCClientReconciler) reconcileNormal(ctx context.Context, oc *tsidpv1alpha1.OIDCClient) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Whitespace or control characters in a redirect URI can never be
	// valid: tsidp's /edit wire format is newline-separated and TrimSpaces
	// each line (which also strips \v, \f, and unicode spaces), so such a
	// spec could register but never converge — and a leading control rune
	// can mask a dangerous scheme past prefix-matching validators. Match
	// tsidp's own normalization semantics exactly. Enforced here rather
	// than in CRD CEL because escapes do not survive the marker->YAML->CEL
	// round trip.
	for _, u := range oc.Spec.RedirectURIs {
		for _, c := range u {
			if unicode.IsSpace(c) || unicode.IsControl(c) {
				return r.notReady(oc, "SpecInvalid", fmt.Sprintf("redirect URI %q contains whitespace or control characters", u))
			}
		}
	}

	// Discovery is fetched before any mutating call so a dead tsidp fails
	// the reconcile before side effects, and so the Secret always carries
	// the public issuer even though the operator may be talking over
	// loopback.
	disc, err := r.Tsidp.Discovery(ctx)
	if err != nil {
		return r.notReady(oc, "TsidpUnreachable", fmt.Sprintf("fetching discovery document: %v", err))
	}
	oc.Status.Issuer = disc.Issuer

	// Renaming spec.secretName deliberately reuses the existing
	// SecretLost -> re-register flow (new credentials at the new name, with
	// a Warning event) instead of a bespoke migration. Only the stale old
	// Secret needs removing here — but the rename target must be vetted
	// FIRST: if it is squatted by a Secret we do not own, stop before
	// destroying the old credentials or the registration.
	if oc.Status.SecretName != "" && oc.Status.SecretName != secretName(oc) {
		if _, err := r.secretAdoptable(ctx, oc, secretName(oc)); err != nil {
			return r.notReady(oc, "SecretConflict", err.Error())
		}
		// The old Secret gets the same scrutiny as the new one: delete it
		// only if it is still controller-owned by this CR (someone may
		// have recreated an unrelated Secret at that name), and pin the
		// delete to the observed UID so a racing recreate survives.
		var old corev1.Secret
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: oc.Status.SecretName}, &old)
		switch {
		case apierrors.IsNotFound(err):
			// Nothing to clean up.
		case err != nil:
			return r.notReady(oc, "SecretCleanupFailed", fmt.Sprintf("reading old Secret %s: %v", oc.Status.SecretName, err))
		default:
			if ref := metav1.GetControllerOf(&old); ref != nil && ref.UID == oc.UID {
				if err := r.Delete(ctx, &old, client.Preconditions{UID: &old.UID}); err != nil && !apierrors.IsNotFound(err) {
					return r.notReady(oc, "SecretCleanupFailed", fmt.Sprintf("deleting old Secret %s: %v", oc.Status.SecretName, err))
				}
			}
			// Not ours (anymore): leave it alone.
		}
		oc.Status.SecretName = ""
	}

	if oc.Status.ClientID == "" {
		outcome, err := r.registerOrAdopt(ctx, oc, disc)
		if err != nil {
			reason := "RegistrationFailed"
			if errors.Is(err, errSecretConflict) {
				reason = "SecretConflict"
			}
			return r.notReady(oc, reason, err.Error())
		}
		switch outcome {
		case outcomeBurnedOrphan:
			return ctrl.Result{RequeueAfter: time.Second}, nil
		case outcomeRegistered:
			// Freshly registered: the Secret was just written with the
			// credentials in hand, and the remote state was just created
			// from spec. Do NOT re-verify through reads in the same pass —
			// the informer cache may still be one write behind our own
			// Secret write, and acting on that stale read would burn the
			// registration we just created.
			r.markReady(oc)
			return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
		}
		// outcomeAdopted falls through: an adopted registration may have
		// drifted while unowned.
	}

	remote, err := r.Tsidp.GetClient(ctx, oc.Status.ClientID)
	if errors.Is(err, tsidp.ErrNotFound) {
		// tsidp lost its state (e.g. restart without the persistent
		// volume). Re-register with fresh credentials.
		r.Recorder.Eventf(oc, corev1.EventTypeWarning, "RegistrationLost",
			"client %s no longer exists in tsidp; re-registering with new credentials", oc.Status.ClientID)
		oc.Status.ClientID = ""
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return r.notReady(oc, "TsidpUnreachable", fmt.Sprintf("reading client: %v", err))
	}

	// The plaintext client_secret lives only in the Secret; if it is gone
	// or points at a different client, it cannot be re-read from tsidp.
	// Burn the registration and start over. This read gates a destructive
	// action, so it MUST bypass the informer cache: only an authoritative
	// NotFound/mismatch from the API server may trigger the burn, and a
	// transient read error must back off instead.
	var sec corev1.Secret
	secErr := r.APIReader.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: secretName(oc)}, &sec)
	if secErr != nil && !apierrors.IsNotFound(secErr) {
		return r.notReady(oc, "SecretReadFailed", fmt.Sprintf("reading credentials Secret: %v", secErr))
	}
	secretOK := secErr == nil && string(sec.Data["client_id"]) == oc.Status.ClientID && len(sec.Data["client_secret"]) > 0
	if !secretOK {
		r.Recorder.Eventf(oc, corev1.EventTypeWarning, "SecretLost",
			"credentials Secret %s/%s is missing or inconsistent; re-registering with new credentials",
			oc.Namespace, secretName(oc))
		if err := r.Tsidp.DeleteClient(ctx, oc.Status.ClientID); err != nil && !errors.Is(err, tsidp.ErrNotFound) {
			return r.notReady(oc, "TsidpUnreachable", fmt.Sprintf("deleting stale client: %v", err))
		}
		oc.Status.ClientID = ""
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Drift: only name and redirect URIs are visible through tsidp's read
	// API; drift in other metadata (scopes, grant types, ...) is
	// undetectable and only converges through Recreate.
	wantName, wantURIs := registeredName(oc), oc.Spec.RedirectURIs
	if remote.ClientName != wantName || !sameStringSet(remote.RedirectURIs, wantURIs) {
		switch oc.Spec.UpdatePolicy {
		case tsidpv1alpha1.UpdatePolicyInPlace:
			// UpdateClient verifies convergence by reading back — tsidp's
			// /edit reports validation failures at HTTP 200.
			if err := r.Tsidp.UpdateClient(ctx, oc.Status.ClientID, wantName, wantURIs); err != nil {
				return r.notReady(oc, "InPlaceUpdateFailed", err.Error())
			}
			r.Recorder.Event(oc, corev1.EventTypeNormal, "UpdatedInPlace",
				"updated client name/redirect URIs in tsidp; client_id and secret preserved")
		case tsidpv1alpha1.UpdatePolicyBlock:
			return r.notReady(oc, "DriftBlocked",
				"registration drifted from spec and updatePolicy is Block; resolve manually or change the policy")
		default: // Recreate
			r.Recorder.Event(oc, corev1.EventTypeWarning, "RecreatingOnDrift",
				"spec drifted from tsidp registration; re-registering with new credentials (updatePolicy Recreate)")
			if err := r.Tsidp.DeleteClient(ctx, oc.Status.ClientID); err != nil && !errors.Is(err, tsidp.ErrNotFound) {
				return r.notReady(oc, "TsidpUnreachable", fmt.Sprintf("deleting drifted client: %v", err))
			}
			oc.Status.ClientID = ""
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	// Keep non-credential Secret content (issuer, endpoints, redirect URIs,
	// template metadata) converged without ever touching the credentials.
	if err := r.upsertSecret(ctx, oc, func(s *corev1.Secret) {
		setSecretMetadata(s, disc, oc.Spec.RedirectURIs)
	}); err != nil {
		return r.notReady(oc, "SecretWriteFailed", err.Error())
	}

	r.markReady(oc)
	log.V(1).Info("reconciled", "clientID", oc.Status.ClientID)
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

func (r *OIDCClientReconciler) markReady(oc *tsidpv1alpha1.OIDCClient) {
	oc.Status.SecretName = secretName(oc)
	setCond(oc, metav1.ConditionTrue, "Ready", "client registered in tsidp; credentials Secret in sync")
}

// secretAdoptable returns an error unless the named Secret is absent or
// already controller-owned by this OIDCClient. It reads through APIReader:
// this guards overwriting user data, so a stale cache must not vouch for
// absence. Returns the Secret when it exists and is ours.
func (r *OIDCClientReconciler) secretAdoptable(ctx context.Context, oc *tsidpv1alpha1.OIDCClient, name string) (*corev1.Secret, error) {
	var sec corev1.Secret
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Secret %s: %w", name, err)
	}
	ref := metav1.GetControllerOf(&sec)
	if ref == nil || ref.UID != oc.UID {
		owner := "no controller"
		if ref != nil {
			owner = fmt.Sprintf("%s %s", ref.Kind, ref.Name)
		}
		return nil, fmt.Errorf("%w: Secret %s/%s already exists and is not owned by this OIDCClient (%s); delete it or choose a different spec.secretName", errSecretConflict, oc.Namespace, name, owner)
	}
	return &sec, nil
}

// registerOrAdopt establishes status.ClientID: it first tries to adopt an
// existing fingerprint-matched registration (recovering from a crash between
// registration and status write), else registers a new client. The Secret is
// written BEFORE status is recorded — the plaintext secret exists nowhere
// else, and the fingerprint pass makes the crash window recoverable.
func (r *OIDCClientReconciler) registerOrAdopt(ctx context.Context, oc *tsidpv1alpha1.OIDCClient, disc *tsidp.ProviderMetadata) (registerOutcome, error) {
	// Refuse to write into a Secret we do not own BEFORE creating remote
	// state, so a naming collision cannot leak registrations or clobber
	// user data.
	existingSec, err := r.secretAdoptable(ctx, oc, secretName(oc))
	if err != nil {
		return outcomeInvalid, err
	}

	fp := Fingerprint(oc)
	list, err := r.Tsidp.ListClients(ctx)
	if err != nil {
		return outcomeInvalid, fmt.Errorf("listing clients for adoption: %w", err)
	}
	for i := range list {
		if !strings.Contains(list[i].ClientName, fp) {
			continue
		}
		// A registration already carries our fingerprint. Adoptable only
		// if our Secret still holds its credentials — tsidp never returns
		// the secret after creation.
		if existingSec != nil && string(existingSec.Data["client_id"]) == list[i].ClientID && len(existingSec.Data["client_secret"]) > 0 {
			oc.Status.ClientID = list[i].ClientID
			r.Recorder.Eventf(oc, corev1.EventTypeNormal, "Adopted",
				"adopted existing tsidp registration %s", list[i].ClientID)
			return outcomeAdopted, nil
		}
		if err := r.Tsidp.DeleteClient(ctx, list[i].ClientID); err != nil && !errors.Is(err, tsidp.ErrNotFound) {
			return outcomeInvalid, fmt.Errorf("deleting unadoptable orphan %s: %w", list[i].ClientID, err)
		}
		r.Recorder.Eventf(oc, corev1.EventTypeWarning, "OrphanReplaced",
			"deleted fingerprint-matched registration %s whose credentials were unrecoverable", list[i].ClientID)
		return outcomeBurnedOrphan, nil
	}

	resp, err := r.Tsidp.Register(ctx, tsidp.RegistrationRequest{
		RedirectURIs:            oc.Spec.RedirectURIs,
		TokenEndpointAuthMethod: oc.Spec.TokenEndpointAuthMethod,
		GrantTypes:              oc.Spec.GrantTypes,
		ResponseTypes:           oc.Spec.ResponseTypes,
		ClientName:              registeredName(oc),
		ClientURI:               oc.Spec.ClientURI,
		LogoURI:                 oc.Spec.LogoURI,
		Scope:                   oc.Spec.Scope,
		Contacts:                oc.Spec.Contacts,
		ApplicationType:         oc.Spec.ApplicationType,
	})
	if err != nil {
		return outcomeInvalid, fmt.Errorf("registering client: %w", err)
	}

	if err := r.upsertSecret(ctx, oc, func(s *corev1.Secret) {
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data["client_id"] = []byte(resp.ClientID)
		s.Data["client_secret"] = []byte(resp.ClientSecret)
		setSecretMetadata(s, disc, oc.Spec.RedirectURIs)
	}); err != nil {
		return outcomeInvalid, fmt.Errorf("writing credentials Secret: %w", err)
	}

	oc.Status.ClientID = resp.ClientID
	oc.Status.SecretName = secretName(oc)
	r.Recorder.Eventf(oc, corev1.EventTypeNormal, "Registered",
		"registered client %s with tsidp; credentials written to Secret %s/%s",
		resp.ClientID, oc.Namespace, secretName(oc))
	return outcomeRegistered, nil
}

func (r *OIDCClientReconciler) upsertSecret(ctx context.Context, oc *tsidpv1alpha1.OIDCClient, mutate func(*corev1.Secret)) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.CreationTimestamp.IsZero() {
			// Creating. Nothing to check: secretAdoptable vetted collisions.
		} else if ref := metav1.GetControllerOf(sec); ref == nil || ref.UID != oc.UID {
			return fmt.Errorf("refusing to overwrite Secret %s/%s: not owned by this OIDCClient", oc.Namespace, sec.Name)
		}
		applySecretTemplate(sec, oc.Spec.SecretTemplate)
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		for k, v := range ManagedSecretLabels {
			sec.Labels[k] = v
		}
		mutate(sec)
		return controllerutil.SetControllerReference(oc, sec, r.Scheme)
	})
	return err
}

// appliedTemplateKeys is the JSON shape stored in templateKeysAnnotation.
type appliedTemplateKeys struct {
	Labels      []string `json:"labels,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
}

// applySecretTemplate converges the Secret's template-managed metadata:
// keys applied by a previous template but absent from the current one are
// removed, so template edits converge instead of accumulating.
func applySecretTemplate(sec *corev1.Secret, tpl *tsidpv1alpha1.SecretTemplate) {
	var prev appliedTemplateKeys
	if raw, ok := sec.Annotations[templateKeysAnnotation]; ok {
		_ = json.Unmarshal([]byte(raw), &prev)
	}

	var cur appliedTemplateKeys
	curLabels := map[string]string{}
	curAnnotations := map[string]string{}
	if tpl != nil {
		curLabels = tpl.Labels
		curAnnotations = tpl.Annotations
	}
	for _, k := range prev.Labels {
		if _, keep := curLabels[k]; !keep {
			delete(sec.Labels, k)
		}
	}
	for _, k := range prev.Annotations {
		if _, keep := curAnnotations[k]; !keep {
			delete(sec.Annotations, k)
		}
	}
	for k, v := range curLabels {
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[k] = v
		cur.Labels = append(cur.Labels, k)
	}
	for k, v := range curAnnotations {
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[k] = v
		cur.Annotations = append(cur.Annotations, k)
	}
	sort.Strings(cur.Labels)
	sort.Strings(cur.Annotations)
	if len(cur.Labels) == 0 && len(cur.Annotations) == 0 {
		delete(sec.Annotations, templateKeysAnnotation)
		return
	}
	raw, _ := json.Marshal(cur)
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Annotations[templateKeysAnnotation] = string(raw)
}

func setSecretMetadata(s *corev1.Secret, disc *tsidp.ProviderMetadata, redirectURIs []string) {
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data["issuer"] = []byte(disc.Issuer)
	s.Data["authorization_endpoint"] = []byte(disc.AuthorizationEndpoint)
	s.Data["token_endpoint"] = []byte(disc.TokenEndpoint)
	s.Data["userinfo_endpoint"] = []byte(disc.UserInfoEndpoint)
	s.Data["jwks_uri"] = []byte(disc.JWKSURI)
	s.Data["redirect_uris"] = []byte(strings.Join(redirectURIs, "\n"))
}

// notReady records a failure on the Ready condition and schedules a fixed
// 30s retry, deliberately returning a NIL error: these are expected external
// conditions (tsidp down, a squatting Secret) already surfaced on the object
// via condition + Event, not controller bugs — returning them as reconcile
// errors would spam error metrics and logs with states the operator cannot
// fix by retrying harder. The trade-off is that
// controller_runtime_reconcile_errors_total stays flat; alert on the Ready
// condition instead.
func (r *OIDCClientReconciler) notReady(oc *tsidpv1alpha1.OIDCClient, reason, msg string) (ctrl.Result, error) {
	setCond(oc, metav1.ConditionFalse, reason, msg)
	return ctrl.Result{RequeueAfter: errBackoff}, nil
}

func setCond(oc *tsidpv1alpha1.OIDCClient, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&oc.Status.Conditions, metav1.Condition{
		Type:               tsidpv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: oc.Generation,
	})
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

// SetupWithManager wires the controller: it owns the credential Secrets, so
// out-of-band Secret deletion triggers reconciliation. main.go scopes the
// Secret informer to ManagedSecretLabels, so only operator-managed Secrets
// are watched and cached.
func (r *OIDCClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tsidpv1alpha1.OIDCClient{}).
		Owns(&corev1.Secret{}).
		Named("oidcclient").
		Complete(r)
}
