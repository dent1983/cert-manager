/*
Copyright 2020 The Jetstack cert-manager contributors.

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

package create

import (
	"context"
	"encoding/pem"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	restclient "k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	apiutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmapiv1alpha2 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	"github.com/jetstack/cert-manager/pkg/util/pki"
	"github.com/jetstack/cert-manager/pkg/webhook"
)

var (
	long = templates.LongDesc(i18n.T(`
Create a cert-manager CertificateRequest resource for one-time Certificate issuing without auto renewal.`))

	example = templates.Examples(i18n.T(`
`))

	alias = []string{"cr"}
)

var (
	// Use the webhook's scheme as it already has the internal cert-manager types,
	// and their conversion functions registered.
	// In future we may we want to consider creating a dedicated scheme used by
	// the ctl tool.
	scheme = webhook.Scheme
)

// Options is a struct to support create certificaterequest command
type Options struct {
	CMClient   cmclient.Interface
	RESTConfig *restclient.Config

	resource.FilenameOptions
	genericclioptions.IOStreams
}

// NewOptions returns initialized Options
func NewOptions(ioStreams genericclioptions.IOStreams) *Options {
	return &Options{
		IOStreams: ioStreams,
	}
}

// NewCmdCreateCertficate returns a cobra command for create CertificateRequest
func NewCmdCreateCertficate(ioStreams genericclioptions.IOStreams, factory cmdutil.Factory) *cobra.Command {
	o := NewOptions(ioStreams)
	cmd := &cobra.Command{
		Use:     "certificaterequest",
		Aliases: alias,
		Short:   "Create a CertificateRequest resource",
		Long:    long,
		Example: example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(factory))
			cmdutil.CheckErr(o.Run(factory, args))
		},
	}

	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, "Path to a the manifest of Certificate resource.")

	return cmd
}

// Complete takes the command arguments and factory and infers any remaining options.
func (o *Options) Complete(f cmdutil.Factory) error {
	var err error

	err = o.FilenameOptions.RequireFilenameOrKustomize()
	if err != nil {
		return err
	}

	o.RESTConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}

	o.CMClient, err = cmclient.NewForConfig(o.RESTConfig)
	if err != nil {
		return err
	}

	return nil
}

// Run executes create certificaterequest command
func (o *Options) Run(f cmdutil.Factory, args []string) error {
	builder := new(resource.Builder)

	cmdNamespace, enforceNamespace, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	r := builder.
		WithScheme(scheme, schema.GroupVersion{Group: cmapiv1alpha2.SchemeGroupVersion.Group, Version: runtime.APIVersionInternal}).
		LocalParam(true).ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, &o.FilenameOptions).Flatten().Do()

	if err := r.Err(); err != nil {
		return fmt.Errorf("error here: %s", err)
	}

	singleItemImplied := false
	infos, err := r.IntoSingleItemImplied(&singleItemImplied).Infos()
	if err != nil {
		return fmt.Errorf("error here instead: %s", err)
	}

	if len(infos) == 0 {
		return fmt.Errorf("no object passed to create certificaterequest")
	}
	if len(infos) > 1 {
		return fmt.Errorf("multiple objects passed to create certificaterequest")
	}

	for _, info := range infos {
		crtObj, err := scheme.ConvertToVersion(info.Object, cmapiv1alpha2.SchemeGroupVersion)
		if err != nil {
			return fmt.Errorf("failed to convert certificate into version v1alpha2: %v", err)
		}

		crt, ok := crtObj.(*cmapiv1alpha2.Certificate)
		if !ok {
			return fmt.Errorf("decoded object is not a v1alpha2 Certificate")
		}

		fmt.Printf("Finally, decoded the object: %#v", crt)

		expectedReqName, err := apiutil.ComputeCertificateRequestName(crt)
		if err != nil {
			return fmt.Errorf("internal error hashing certificate spec: %v", err)
		}

		signer, err := pki.GeneratePrivateKeyForCertificate(crt)
		if err != nil {
			return fmt.Errorf("error when generating private key")
		}

		keyData, err := pki.EncodePrivateKey(signer, crt.Spec.KeyEncoding)
		if err != nil {
			return fmt.Errorf("error when encoding private key")
		}

		req, err := o.buildCertificateRequest(crt, expectedReqName, keyData)
		if err != nil {
			return err
		}

		ns := crt.Namespace
		if ns == "" {
			ns = cmdNamespace
		}
		req, err = o.CMClient.CertmanagerV1alpha2().CertificateRequests(ns).Create(context.TODO(), req, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("error when creating CertifcateRequest through client: %v", err)
		}
	}

	return nil
}

func (o *Options) buildCertificateRequest(crt *cmapiv1alpha2.Certificate, name string, pk []byte) (*cmapiv1alpha2.CertificateRequest, error) {
	csrPEM, err := generateCSR(crt, pk)
	if err != nil {
		return nil, err
	}

	annotations := make(map[string]string, len(crt.Annotations)+2)
	for k, v := range crt.Annotations {
		annotations[k] = v
	}
	annotations[cmapiv1alpha2.CRPrivateKeyAnnotationKey] = crt.Spec.SecretName
	annotations[cmapiv1alpha2.CertificateNameKey] = crt.Name

	cr := &cmapiv1alpha2.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: crt.Name + "-",
			Annotations:  annotations,
			Labels:       crt.Labels,
		},
		Spec: cmapiv1alpha2.CertificateRequestSpec{
			CSRPEM:    csrPEM,
			Duration:  crt.Spec.Duration,
			IssuerRef: crt.Spec.IssuerRef,
			IsCA:      crt.Spec.IsCA,
			Usages:    crt.Spec.Usages,
		},
	}

	return cr, nil
}

func generateCSR(crt *cmapiv1alpha2.Certificate, pk []byte) ([]byte, error) {
	csr, err := pki.GenerateCSR(crt)
	if err != nil {
		return nil, err
	}

	signer, err := pki.DecodePrivateKeyBytes(pk)
	if err != nil {
		return nil, err
	}

	csrDER, err := pki.EncodeCSR(csr, signer)
	if err != nil {
		return nil, err
	}

	csrPEM := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE REQUEST", Bytes: csrDER,
	})

	return csrPEM, nil
}
