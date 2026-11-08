(function () {
    'use strict';
    var fields = [
        ['enabled','启用模型','checkbox'], ['base_url','API BaseURL','text'],
        ['model','模型 ID','text'], ['api_key','API Key','password'],
        ['daily_token_limit','每日 Token 额度','number',1,1000000000]
    ];
    var form=document.getElementById('model-settings-form'); if(!form)return;
    var box=document.getElementById('model-settings-inputs'), group=document.getElementById('model-settings-fields'), msg=document.getElementById('model-settings-msg');
    var value=null,dirty=false,busy=false,seq=0;
    fields.forEach(function(f){
        var label=document.createElement('label');label.textContent=f[1];label.style.cssText='display:flex;flex-direction:column;gap:6px';
        var input=document.createElement('input');input.id='model-setting-'+f[0];input.name=f[0];input.type=f[2];
        if(f[2]==='number'){input.min=f[3];input.max=f[4];input.step='1';input.required=true;}
        if(f[2]==='text')input.autocomplete='off';
        if(f[0]==='api_key'){input.autocomplete='new-password';input.maxLength=4096;input.placeholder='填写 API Key';}
        input.addEventListener('input',function(){dirty=true;seq++;msg.textContent='有未保存的修改';});label.appendChild(input);box.appendChild(label);
    });
    function render(v){value=v;fields.forEach(function(f){var el=form.elements.namedItem(f[0]);if(f[2]==='checkbox')el.checked=v[f[0]]===true;else el.value=f[0]==='api_key'?'':v[f[0]];});dirty=false;group.disabled=false;form.elements.namedItem('api_key').placeholder=v.key_configured?'已保存，留空不修改':'填写 API Key';var u=v.daily_usage;msg.textContent=u?'今日已用 '+u.used_tokens.toLocaleString()+' / '+u.limit.toLocaleString()+' Token，预留或待确认 '+u.reserved_tokens.toLocaleString()+'。北京时间每日 00:00 重置。':'每日额度统计输入与输出 Token，北京时间每日 00:00 重置。';}
    window.resetModelSettingsSession=function(){seq++;value=null;dirty=false;busy=false;group.disabled=true;fields.forEach(function(f){var el=form.elements.namedItem(f[0]);el.value='';el.checked=false;});msg.textContent='请重新加载当前会话的配置。';};
    window.fetchModelSettings=async function(force){
        if(busy || dirty&&!force)return;
        if(force&&dirty&&!window.confirm('重新加载会丢弃未保存的修改，是否继续？'))return;
        var n=++seq,currentView=captureMonitorView();var current=function(){return n===seq&&currentView();};
        var resp=await apiFetch('/api/settings/model',{current:current});if(!current())return;
        if(!resp||!resp.ok){msg.textContent='读取失败，未覆盖当前输入。请重新进入设置或重新加载。';return;}
        try{var v=await resp.json();if(current())render(v);}catch(_){if(current())msg.textContent='模型配置响应无效';}
    };
    document.getElementById('model-settings-reload').onclick=function(){window.fetchModelSettings(true);};
    form.onsubmit=async function(e){
        e.preventDefault();if(busy||!value||!form.reportValidity())return;
        var payload=Object.assign({},value,{currency:"",price_version:"",input_price_micros:0,output_price_micros:0,max_run_micros:0});delete payload.daily_usage;payload.api_key_env='';var invalid=false;
        fields.forEach(function(f){var el=form.elements.namedItem(f[0]),v=el.value;
            if(f[2]==='checkbox')v=el.checked;else if(f[2]==='number')v=Number(v);
            if(f[2]==='number'&&!Number.isSafeInteger(v))invalid=true;payload[f[0]]=v;});
        if(invalid){msg.textContent='数值无效';return;}
        if(payload.enabled&&!value.key_configured&&!payload.api_key){msg.textContent='请填写 API Key';return;}
        busy=true;group.disabled=true;var n=++seq,view=captureMonitorView();
        try {var resp=await apiRequest('/api/settings/model',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload),rawHTTPError:true,current:function(){return n===seq&&view();}});
            if(n!==seq||!view())return;
            if(!resp||!resp.ok){msg.textContent=resp&&resp.status===409?'其他页面已更新配置，请重新加载后再保存。':(!resp||resp.status>=500?'保存结果未确认，请重新加载检查修订。':'保存被拒绝，请检查连接信息、API Key 与每日 Token 额度。');return;}
            var saved=await resp.json();if(n===seq&&view())render(saved);
        } catch(_){if(n===seq&&view())msg.textContent='保存结果未确认，请重新加载检查修订。';}
        finally {if(n===seq){busy=false;group.disabled=!value;}}
    };
})();
