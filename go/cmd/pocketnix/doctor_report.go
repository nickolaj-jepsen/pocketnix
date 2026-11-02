package main

import (
	"reflect"
	"strings"
)

// doctorSchema is doctorReport.Schema; bump it when a field changes meaning.
const doctorSchema = 1

// doctorReport is what `pocketnix doctor --json` prints. A nil pointer is a
// fact doctor couldn't read: it is never guessed. It holds named props only,
// never the adb serial, account names or the phone number.
type doctorReport struct {
	Schema  int           `json:"schema"`
	Device  doctorDevice  `json:"device"`
	AVF     doctorAVF     `json:"avf"`
	App     doctorApp     `json:"app"`
	Verdict doctorVerdict `json:"verdict"`
	// Smoke is absent unless --smoke ran.
	Smoke []smokeStep `json:"smoke,omitempty"`
}

type doctorDevice struct {
	Fingerprint  *string `json:"fingerprint"`
	Model        *string `json:"model"`
	Manufacturer *string `json:"manufacturer"`
	SoC          *string `json:"soc"`
	SDK          *string `json:"sdk"`
	Release      *string `json:"release"`
	// OS is one of the os* constants.
	OS     string  `json:"os"`
	Kernel *string `json:"kernel"`
}

const (
	osGrapheneOS = "grapheneos"
	osStock      = "stock"
	osUnknown    = "unknown"
)

type doctorAVF struct {
	Feature              *bool   `json:"feature"`
	VMSupported          *bool   `json:"vmSupported"`
	ProtectedVMSupported *bool   `json:"protectedVmSupported"`
	HypervisorVersion    *string `json:"hypervisorVersion"`
	ApexVersion          *string `json:"apexVersion"`
	UBoot                bool    `json:"uboot"`
	// VMInfo is `vm info`'s output lines; empty, not nil, when it printed none.
	VMInfo []string `json:"vmInfo"`
}

type doctorApp struct {
	Package     string  `json:"package"`
	Installed   bool    `json:"installed"`
	VersionName *string `json:"versionName"`
	// Caps is ProvisionProvider's caps answer, nil when there's none.
	Caps map[string]string `json:"caps"`
	// CapsError is why Caps is nil, empty when it isn't.
	CapsError string `json:"capsError"`
}

type doctorVerdict struct {
	// Level is one of the level* constants.
	Level string `json:"level"`
	// Reasons is never nil, so an ok verdict prints [].
	Reasons []string `json:"reasons"`
}

const (
	levelOK   = "ok"
	levelWarn = "warn"
	levelFail = "fail"
)

type smokeStep struct {
	Name string `json:"name"`
	// OK is nil when the step didn't apply; Detail says why.
	OK     *bool  `json:"ok"`
	Detail string `json:"detail"`
}

// redactStrings replaces the secrets in every string v reaches through
// pointers, structs, slices and map values. Its structs' fields must be exported.
func redactStrings(v reflect.Value, redact *strings.Replacer) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(redact.Replace(v.String()))
	case reflect.Pointer:
		if !v.IsNil() {
			redactStrings(v.Elem(), redact)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			redactStrings(v.Field(i), redact)
		}
	case reflect.Slice:
		for i := range v.Len() {
			redactStrings(v.Index(i), redact)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			value := reflect.New(v.Type().Elem()).Elem()
			value.Set(it.Value())
			redactStrings(value, redact)
			v.SetMapIndex(it.Key(), value)
		}
	}
}
