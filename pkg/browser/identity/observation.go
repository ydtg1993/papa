package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

type AdapterObservation struct {
	ID         string `json:"id,omitempty"`
	Version    string `json:"version,omitempty"`
	Enabled    bool   `json:"enabled"`
	Registered bool   `json:"registered"`
	Executed   bool   `json:"executed"`
	Verified   bool   `json:"verified"`
	Reason     string `json:"reason,omitempty"`
}
type FontObservation struct {
	Available  bool     `json:"available"`
	Candidates []string `json:"candidates"`
	Matched    []string `json:"matched"`
	Reason     string   `json:"reason,omitempty"`
}
type WebGLObservation struct {
	Available bool   `json:"available"`
	Vendor    string `json:"vendor,omitempty"`
	Renderer  string `json:"renderer,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type Observation struct {
	CheckerVersion       string                            `json:"checker_version"`
	Reference            Reference                         `json:"reference"`
	Runtime              RuntimeIdentity                   `json:"runtime"`
	UserAgent            string                            `json:"user_agent"`
	Platform             string                            `json:"platform"`
	Language             string                            `json:"language"`
	Languages            []string                          `json:"languages"`
	Locale               string                            `json:"locale"`
	Timezone             string                            `json:"timezone"`
	Viewport             Size                              `json:"viewport"`
	Screen               Size                              `json:"screen"`
	DeviceScaleFactor    float64                           `json:"device_scale_factor"`
	MaxTouchPoints       int                               `json:"max_touch_points"`
	HardwareConcurrency  int                               `json:"hardware_concurrency"`
	UserAgentMetadata    *proto.EmulationUserAgentMetadata `json:"user_agent_metadata,omitempty"`
	ClientHintsAvailable bool                              `json:"client_hints_available"`
	ClientHintsReason    string                            `json:"client_hints_reason,omitempty"`
	Webdriver            bool                              `json:"webdriver"`
	AdapterGetter        string                            `json:"adapter_getter,omitempty"`
	Adapter              AdapterObservation                `json:"adapter"`
	Fonts                FontObservation                   `json:"fonts"`
	CanvasHash           string                            `json:"canvas_hash,omitempty"`
	CanvasAvailable      bool                              `json:"canvas_available"`
	CanvasReason         string                            `json:"canvas_reason,omitempty"`
	WebGL                WebGLObservation                  `json:"webgl"`
	Coverage             []string                          `json:"coverage"`
}

// FirstScript is a synchronous, bounded probe suitable for the very first
// inline script in an isolated diagnostic fixture. It collects no cookies,
// storage, URLs or credentials. UA-CH here reports its low-entropy values.
const FirstScript = `() => {
 const u=navigator.userAgentData;
 return {user_agent:navigator.userAgent,platform:navigator.platform,language:navigator.language,languages:Array.from(navigator.languages),locale:Intl.DateTimeFormat().resolvedOptions().locale,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone,viewport:{width:innerWidth,height:innerHeight},screen:{width:screen.width,height:screen.height},device_scale_factor:devicePixelRatio,max_touch_points:navigator.maxTouchPoints,hardware_concurrency:navigator.hardwareConcurrency,webdriver:navigator.webdriver,adapter_getter:Object.getOwnPropertyDescriptor(Navigator.prototype,'webdriver')?.get?.name||'',client_hints_available:!!u,user_agent_metadata:u?{brands:u.brands,mobile:u.mobile,platform:u.platform}:null};
}`

// Observe reads actual values; unavailable probes remain explicitly unavailable.
// Registered is set only by the managed-page owner, never inferred here.
func Observe(page *rod.Page, resolved *ResolvedProfile) (*Observation, error) {
	if page == nil {
		return nil, fmt.Errorf("identity observation page required")
	}
	r := resolved.Clone()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	result, err := page.Eval(`async () => {
 const out=(` + FirstScript + `)();
 if(navigator.userAgentData){try {Object.assign(out.user_agent_metadata,await navigator.userAgentData.getHighEntropyValues(['architecture','bitness','model','platformVersion','fullVersionList','wow64']));}catch(e){out.client_hints_reason='high entropy unavailable';}}
 else out.client_hints_reason='API unavailable in this context';
 out.fonts={available:false,candidates:['Arial','Times New Roman','Courier New','Segoe UI','Noto Sans','Liberation Sans'],matched:[]};
 try {const c=document.createElement('canvas'),x=c.getContext('2d');if(!x)throw 0;const s='mmmmmmmmmmlliWW123';for(const f of out.fonts.candidates){let different=false;for(const base of ['monospace','serif','sans-serif']){x.font='72px '+base;const w=x.measureText(s).width;x.font='72px "'+f+'",'+base;if(x.measureText(s).width!==w)different=true;}if(different)out.fonts.matched.push(f);}out.fonts.available=true;}catch(e){out.fonts.reason='bounded font sample unavailable';}
 try {const c=document.createElement('canvas');c.width=240;c.height=48;const x=c.getContext('2d');if(!x)throw 0;x.fillStyle='#f4a';x.fillRect(2,2,73,19);x.font='16px sans-serif';x.fillStyle='#135';x.fillText('Papa native canvas 0123',5,30);out.canvas_data=c.toDataURL();out.canvas_available=true;}catch(e){out.canvas_available=false;out.canvas_reason='2D canvas unavailable';}
 out.webgl={available:false};try {const c=document.createElement('canvas'),g=c.getContext('webgl');if(!g)throw 0;const e=g.getExtension('WEBGL_debug_renderer_info');out.webgl={available:true,vendor:String(g.getParameter(e?e.UNMASKED_VENDOR_WEBGL:g.VENDOR)).slice(0,512),renderer:String(g.getParameter(e?e.UNMASKED_RENDERER_WEBGL:g.RENDERER)).slice(0,512)};g.getExtension('WEBGL_lose_context')?.loseContext();}catch(e){out.webgl.reason='WebGL unavailable';}
 return out;
}`)
	if err != nil {
		return nil, err
	}
	var o Observation
	if err = result.Value.Unmarshal(&o); err != nil {
		return nil, err
	}
	var canvas struct {
		Data string `json:"canvas_data"`
	}
	if err = result.Value.Unmarshal(&canvas); err != nil {
		return nil, err
	}
	if o.CanvasAvailable && len(canvas.Data) > 0 && len(canvas.Data) < 128*1024 {
		sum := sha256.Sum256([]byte(canvas.Data))
		o.CanvasHash = hex.EncodeToString(sum[:])
	} else {
		o.CanvasAvailable = false
		o.CanvasReason = "canvas data unavailable or exceeds bound"
	}
	v, err := proto.BrowserGetVersion{}.Call(page)
	if err != nil {
		return nil, err
	}
	o.CheckerVersion = CheckerVersion
	o.Reference = r.Reference
	o.Runtime = RuntimeIdentity{Product: v.Product, Revision: v.Revision, UserAgent: v.UserAgent, JSVersion: v.JsVersion, Platform: o.Platform}
	o.Adapter = AdapterObservation{ID: r.Profile.Adapter.ID, Version: r.Profile.Adapter.Version, Enabled: r.Profile.Adapter.Enabled}
	if o.Adapter.Enabled {
		o.Adapter.Executed = o.AdapterGetter == "papaIdentityWebdriverV1"
		o.Adapter.Verified = o.Adapter.Executed && !o.Webdriver
		if !o.Adapter.Verified {
			o.Adapter.Reason = "registered adapter execution was not verified in this document"
		}
	}
	o.Coverage = []string{"managed page target main document", "same-target frames require separate verification", "popups, out-of-process frames, workers and raw Rod pages are not guaranteed"}
	return &o, nil
}
