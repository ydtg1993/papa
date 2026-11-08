const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const nodes=new Map(),fields=new Map();
function element(){return {value:'',checked:false,disabled:false,textContent:'',events:{},style:{},appendChild(x){if(x.name)fields.set(x.name,x);},addEventListener(k,f){this.events[k]=f;}};}
['model-settings-form','model-settings-inputs','model-settings-fields','model-settings-msg','model-settings-reload'].forEach(k=>nodes.set(k,element()));
const form=nodes.get('model-settings-form');form.elements={namedItem:k=>fields.get(k)};form.reportValidity=()=>true;
let active=true,gets=[],posts=[];
const ctx={document:{getElementById:k=>nodes.get(k),createElement:()=>element()},window:{confirm:()=>true},captureMonitorView:()=>()=>active,apiFetch:()=>new Promise(resolve=>gets.push(resolve)),apiRequest:(url,options)=>{assert.equal(options.rawHTTPError,true);return new Promise(resolve=>posts.push({resolve,payload:JSON.parse(options.body)}));}};
vm.createContext(ctx);vm.runInContext(fs.readFileSync(path.join(process.argv[2],'model-settings.js'),'utf8'),ctx);
const load=ctx.window.fetchModelSettings,resp=v=>({ok:true,json:async()=>v});
const initial={revision:'9007199254740993',enabled:false,base_url:'',model:'',api_key_env:'KEY',key_configured:false,daily_token_limit:1000000,daily_usage:{used_tokens:75,reserved_tokens:0,limit:1000000},request_seconds:30,run_seconds:180,max_model_calls:12,max_candidates_per_stage:3,max_output_tokens:4096,max_input_bytes:131072,max_total_input_bytes:1048576,max_response_bytes:262144,output_token_field:'max_completion_tokens',json_output:false,thinking_mode:'',currency:'CNY',price_version:'',input_price_micros:100001,output_price_micros:0,max_run_micros:0};
(async()=>{
 assert.deepEqual([...fields.keys()],['enabled','base_url','model','api_key','daily_token_limit']);
 assert.equal(fields.get('api_key').type,'password');
 let a=load(),b=load();gets[1](resp(initial));await b;gets[0](resp({...initial,daily_token_limit:1}));await a;
 assert.equal(fields.get('daily_token_limit').value,1000000,'stale GET overwrote config');
 a=load();fields.get('model').value='draft';fields.get('model').events.input();gets[2](resp(initial));await a;assert.equal(fields.get('model').value,'draft');
 await load();assert.equal(gets.length,3,'navigation overwrote dirty edit');
 fields.get('api_key').value='synthetic-private-key';
 a=form.onsubmit({preventDefault(){}});assert.equal(posts[0].payload.api_key,'synthetic-private-key');assert.equal(posts[0].payload.daily_usage,undefined);assert.equal(posts[0].payload.api_key_env,'');assert.equal(posts[0].payload.daily_token_limit,1000000);assert.equal(posts[0].payload.max_output_tokens,4096);assert.equal(posts.length,1);assert.equal(posts[0].payload.revision,initial.revision);assert.equal(posts[0].payload.input_price_micros,0);assert.equal(posts[0].payload.max_run_micros,0);assert.equal(posts[0].payload.output_token_field,initial.output_token_field);assert.equal(posts[0].payload.max_model_calls,12);
 assert.equal(nodes.get('model-settings-fields').disabled,true);await form.onsubmit({preventDefault(){}});assert.equal(posts.length,1,'double save');
 posts[0].resolve({ok:false,status:409});await a;assert.equal(nodes.get('model-settings-fields').disabled,false);assert.match(nodes.get('model-settings-msg').textContent,/重新加载/);assert.equal(fields.get('model').value,'draft');
 a=load(true);gets[3](resp({...initial,revision:'9007199254740994'}));await a;
 assert.equal(fields.get('model').value,'');assert.equal(fields.get('api_key').value,'');
 a=form.onsubmit({preventDefault(){}});active=false;posts[1].resolve(resp({...initial,model:'late'}));await a;
 assert.equal(fields.get('model').value,'','stale POST updated departed view');assert.equal(nodes.get('model-settings-fields').disabled,false);
 ctx.window.resetModelSettingsSession();assert.equal(fields.get('api_key').value,'');assert.equal(nodes.get('model-settings-fields').disabled,true);
 active=true;a=load(true);gets[4](resp({...initial,key_configured:true}));await a;fields.get('api_key').value='replacement-key';a=form.onsubmit({preventDefault(){}});posts[2].resolve(resp({...initial,key_configured:true}));await a;assert.equal(fields.get('api_key').value,'','saved key retained in input');
 console.log('model settings stale GET, dirty input, conflict, frozen payload, double save and view guards passed');
})().catch(e=>{console.error(e);process.exitCode=1;});
