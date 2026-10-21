package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// filledConfig is a Config with one element in every list and every block, and a distinct path in
// every string setting named *_file but ca_file and cert_file (public material), each in its own
// directory /secrets/<n>. It returns the settings so filled, by name.
func filledConfig(t *testing.T) (Config, map[string]string) {
	t.Helper()
	var c Config
	want := map[string]string{}
	var fill func(v reflect.Value, path string)
	fill = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.Type().Elem().Kind() == reflect.Struct {
				v.Set(reflect.New(v.Type().Elem()))
				fill(v.Elem(), path)
			}
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Struct {
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
				fill(v.Index(0), path+"[0]")
			}
		case reflect.Struct:
			for i := range v.NumField() {
				f := v.Type().Field(i)
				tag := f.Tag.Get("yaml")
				name := strings.Split(tag, ",")[0]
				inline := strings.Contains(tag, ",inline")
				if !f.IsExported() || name == "-" || name == "" && !inline {
					continue
				}
				p := path
				if !inline {
					if p != "" {
						p += "."
					}
					p += name
				}
				fv := v.Field(i)
				if fv.Kind() == reflect.String && strings.HasSuffix(name, "_file") && name != "ca_file" && name != "cert_file" {
					file := fmt.Sprintf("/secrets/%d/f", len(want))
					fv.SetString(file)
					want[p] = file
					continue
				}
				fill(fv, p)
			}
		}
	}
	fill(reflect.ValueOf(&c).Elem(), "")
	if len(want) < 15 {
		t.Fatalf("only %d file settings found: %v", len(want), want)
	}
	return c, want
}

// Every secret-file setting is in SecretFiles, and a kubesecret source's directory holding it is
// refused, a setting at a time.
func TestSecretFilesComplete(t *testing.T) {
	c, want := filledConfig(t)
	got := map[string]string{}
	for _, f := range c.SecretFiles() {
		got[f.Name] = f.Path
	}
	for name, file := range want {
		if got[name] != file {
			t.Errorf("%s (%s) is not in SecretFiles", name, file)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("SecretFiles lists %s, which is no *_file setting", name)
		}
	}
	for name, file := range want {
		t.Run(name, func(t *testing.T) {
			c := c
			c.Signers.FileDir = ""
			c.Signers.Sources = append(slices.Clone(c.Signers.Sources), Source{Name: "k8s", Kind: "kubesecret",
				KubeSecret: &KubeSecret{Dir: filepath.Dir(file), EtcdEncryptionAtRest: true}})
			var errs []string
			validateKubeSecret(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }, "k8s", len(c.Signers.Sources)-1, c, false)
			if len(errs) != 1 || !strings.Contains(errs[0], "holds "+name+":") {
				t.Fatalf("got %v", errs)
			}
		})
	}
}
