/* Browser-local display settings, registered in the existing server settings. */
'use strict';
Papa.settings('appearance', '显示与动效', function (el) {
    var p = document.createElement('p'); p.className = 'hint';
    p.textContent = '偏好只保存于当前浏览器。减弱动效保留静态加载提示，数据立即显示真实值。';
    var label = document.createElement('label'), select = document.createElement('select');
    label.className = 'appearance-control'; label.append(document.createTextNode('动效偏好 '));
    [['system','跟随系统'],['reduced','减弱动效']].forEach(function (v) { var option=document.createElement('option');option.value=v[0];option.textContent=v[1];select.append(option); });
    var motion = typeof Oao !== 'undefined' && Oao.motion;
    if (motion) select.value=motion.get();
    else { select.disabled=true;p.textContent+=' 当前 oao 未提供动效扩展；系统减弱动效设置仍然生效。'; }
    function change() { if (motion) motion.set(select.value); }
    select.addEventListener('change',change); label.append(select);el.append(label,p);
    return function () { select.removeEventListener('change',change); };
});
