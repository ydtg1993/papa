package tasktrace

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestTraceExecutionAndTruncationNotice(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js required")
	}
	source, err := Assets.ReadFile("static/trace.js")
	if err != nil {
		t.Fatal(err)
	}
	js := `let feature,body;
const steps=[{execution_id:'<run>',truncated:true,attempt:0,seq:0,step:'parse',status:'ok'}];
global.MonitorShell={install:()=>{},define:(name,scope,requires,factory)=>{feature=factory({window:{fetch:()=>Promise.resolve({ok:true})},getSessionGeneration:()=>1,esc:s=>String(s).replaceAll('<','&lt;').replaceAll('>','&gt;'),apiFetch:async()=>({json:async()=>({steps})}),Dialog:{open:x=>body=x.body},Toast:{err:x=>{throw Error(x)}}},{current:()=>()=>true,cleanup:()=>{}})}};
` + string(source) + `
(async()=>{
await feature.showTaskTrace('1');
if(!body.includes('&lt;run&gt;')||body.includes('<run>')||!body.includes('最后 500 步'))throw Error(body);
steps[0].execution_id='';steps[0].truncated=false;
await feature.showTaskTrace('2');
if(!body.includes('历史记录无执行轮次标识')||body.includes('最后 500 步'))throw Error(body);
})().catch(e=>{console.error(e);process.exitCode=1});`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, node, "-e", js).CombinedOutput(); err != nil {
		t.Fatalf("trace UI: %v\n%s", err, out)
	}
}
