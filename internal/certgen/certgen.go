package certgen

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Run generates a self-signed CA and serving certificate, writes them to a
// Kubernetes TLS Secret, and patches the caBundle on a MutatingWebhookConfiguration.
// All parameters are read from environment variables set by the Helm cert-gen Job.
func Run(ctx context.Context, logger *slog.Logger) error {
	namespace := os.Getenv("KOSHI_NAMESPACE")
	serviceName := os.Getenv("KOSHI_SERVICE_NAME")
	secretName := os.Getenv("KOSHI_SECRET_NAME")
	webhookName := os.Getenv("KOSHI_WEBHOOK_NAME")

	if namespace == "" || serviceName == "" || secretName == "" || webhookName == "" {
		return fmt.Errorf("required env vars: KOSHI_NAMESPACE=%q, KOSHI_SERVICE_NAME=%q, KOSHI_SECRET_NAME=%q, KOSHI_WEBHOOK_NAME=%q",
			namespace, serviceName, secretName, webhookName)
	}

	dnsName := serviceName + "." + namespace + ".svc"
	logger.Info("generating certificates", "dns_name", dnsName)

	caPEM, caKeyPEM, err := generateCA()
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}

	certPEM, keyPEM, err := generateServingCert(caPEM, caKeyPEM, []string{dnsName})
	if err != nil {
		return fmt.Errorf("generate serving cert: %w", err)
	}

	logger.Info("certificates generated, connecting to cluster")

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	// Create or update the TLS Secret.
	if err := ensureSecret(ctx, client, namespace, secretName, certPEM, keyPEM); err != nil {
		return fmt.Errorf("ensure secret: %w", err)
	}
	logger.Info("TLS secret written", "name", secretName, "namespace", namespace)

	// Patch caBundle on the MutatingWebhookConfiguration.
	if err := patchWebhookCABundle(ctx, client, webhookName, caPEM); err != nil {
		return fmt.Errorf("patch webhook caBundle: %w", err)
	}
	logger.Info("webhook caBundle patched", "name", webhookName)

	return nil
}

func generateCA() (caPEM, caKeyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "koshi-webhook-ca"},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return caPEM, caKeyPEM, nil
}

func generateServingCert(caPEM, caKeyPEM []byte, dnsNames []string) (certPEM, keyPEM []byte, err error) {
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA key PEM")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	servingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(servingKey)})
	return certPEM, keyPEM, nil
}

func ensureSecret(ctx context.Context, client kubernetes.Interface, namespace, name string, certPEM, keyPEM []byte) error {
	secrets := client.CoreV1().Secrets(namespace)

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		// Secret exists — update it.
		existing.Data = map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		}
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}

	// Create new secret.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	return err
}

func patchWebhookCABundle(ctx context.Context, client kubernetes.Interface, name string, caPEM []byte) error {
	webhooks := client.AdmissionregistrationV1().MutatingWebhookConfigurations()

	existing, err := webhooks.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get webhook %q: %w", name, err)
	}

	// Patch caBundle on all webhook entries.
	for i := range existing.Webhooks {
		existing.Webhooks[i].ClientConfig.CABundle = caPEM
	}

	// Use a strategic merge patch with the caBundle set.
	type webhookPatch struct {
		Webhooks []map[string]interface{} `json:"webhooks"`
	}
	var patches []map[string]interface{}
	for _, wh := range existing.Webhooks {
		patches = append(patches, map[string]interface{}{
			"name": wh.Name,
			"clientConfig": map[string]interface{}{
				"caBundle": caPEM,
			},
		})
	}

	patchData, err := json.Marshal(webhookPatch{Webhooks: patches})
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = webhooks.Patch(ctx, name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
	return err
}
