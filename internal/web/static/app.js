'use strict';
const $ = id => document.getElementById(id);
const state = { range:'1h', samples:[], charts:[], system:null, networkInterface:'', coreChart:null, networkChart:null };
const ranges = { '1m':60e3, '3m':3*60e3, '5m':5*60e3, '15m':15*60e3, '1h':3600e3, '6h':6*3600e3, '24h':86400e3, '168h':7*86400e3, '720h':30*86400e3 };
const fmt = (v,n=1) => v == null ? '--' : Number(v).toFixed(n);
const bytes = v => { const units=['B','KiB','MiB','GiB','TiB']; let i=0,n=v||0; while(n>=1024&&i<4){n/=1024;i++} return `${n.toFixed(1)} ${units[i]}`; };

function updateCards(p){
  $('cpu-temp').textContent=fmt(p.cpu_temp_c); $('cpu-freq').textContent=fmt(p.cpu_freq_mhz,0);
  $('cpu-usage').textContent=fmt(p.cpu_usage_pct); $('memory').textContent=fmt(p.memory_pct);
  $('disk').textContent=fmt(p.disk_pct); $('fan').textContent=fmt(p.fan_rpm,0);
  $('nvme-temp').textContent=fmt(p.nvme_temp_c); $('uptime').textContent=fmt(p.uptime_seconds/86400,1);
	if($('load-info')) $('load-info').textContent=`${fmt(p.load_1,2)} / ${fmt(p.load_5,2)} / ${fmt(p.load_15,2)} (1/5/15 min)`;
  $('memory').title=`${bytes(p.memory_used_bytes)} / ${bytes(p.memory_total_bytes)}`;
  const si=state.system||{};
  $('disk-card').title=`Used: ${bytes(p.disk_used_bytes)}\nTotal: ${bytes(p.disk_total_bytes)}\nFilesystem: ${si.disk_filesystem||'unknown'}\nDevice: ${si.disk_device||'unknown'}\nMounted at: ${si.disk_mount||'/'}`;
	$('nvme-card').classList.toggle('hidden',p.nvme_temp_c==null);
	updateCoreTable(p.cpu_core_usage_pct||[]);
	updateNetwork(p);
  $('last-update').textContent=`LAST SAMPLE ${new Date(p.timestamp).toLocaleString()}`;
}

function rate(v){return `${bytes(v||0)}/s`}
function selectedInterfaceInfo(){return (state.system?.network_interfaces||[]).find(v=>v.name===state.networkInterface)}
function updateNetwork(p){
  const point=(p.network||{})[state.networkInterface];
  $('network-rx-rate').textContent=point?rate(point.rx_bytes_per_sec):'--';
  $('network-tx-rate').textContent=point?rate(point.tx_bytes_per_sec):'--';
  $('network-rx-total').textContent=point?bytes(point.rx_bytes):'--';
  $('network-tx-total').textContent=point?bytes(point.tx_bytes):'--';
  const info=selectedInterfaceInfo();
  $('network-meta').textContent=info?`STATE ${String(info.state||'unknown').toUpperCase()}  //  MTU ${info.mtu}  //  MAC ${info.mac||'none'}  //  ADDR ${(info.addresses||[]).join(', ')||'none'}`:'NO INTERFACE DATA';
}

function updateCoreTable(cores){
  $('core-table').innerHTML=cores.map((v,i)=>`<tr><td>CPU${i}</td><td>${fmt(v)}%</td><td><progress class="core-bar" max="100" value="${Math.max(0,Math.min(100,v))}"></progress></td></tr>`).join('')||'<tr><td colspan="3">WAITING FOR SECOND SAMPLE...</td></tr>';
}

async function loadSystem(){
  try{const r=await fetch('/api/system');if(!r.ok)throw Error(r.status);state.system=await r.json();const s=state.system;
    document.title=`${String(s.hostname||'PI').toUpperCase()} // SYSTEM MONITOR`;
    $('host-prompt').textContent=`monitor@${s.hostname||'pi'}:~$`;
    $('nvme-card').classList.toggle('hidden',!s.has_nvme_temp);
    const rows=[['HOSTNAME',s.hostname],['MODEL',s.model||'Unknown'],['OS',s.operating_system],['KERNEL',s.kernel_version],['ARCH',s.architecture],['CPU CORES',s.cpu_cores],['LOAD AVERAGE','<span id="load-info">--</span>'],['ROOT DEVICE',s.disk_device||'Unknown'],['FILESYSTEM',s.disk_filesystem||'Unknown'],['MOUNT',s.disk_mount]];
    $('system-table').innerHTML=rows.map(([k,v])=>`<tr><th>${k}</th><td>${String(v)}</td></tr>`).join('');
		const interfaces=s.network_interfaces||[], preferred=interfaces.find(v=>v.name!=='lo'&&v.state==='up')||interfaces.find(v=>v.name!=='lo')||interfaces[0];
		const select=$('network-select');select.replaceChildren();for(const iface of interfaces){const option=document.createElement('option');option.value=iface.name;option.textContent=`${iface.name} [${iface.state||'unknown'}]`;select.append(option)}
		state.networkInterface=preferred?.name||'';select.value=state.networkInterface;select.onchange=()=>{state.networkInterface=select.value;if(state.samples.length)updateNetwork(state.samples.at(-1));redraw()};
		if(state.samples.length){updateCards(state.samples.at(-1));redraw()}
  }catch(e){console.error(e)}
}

class RetroChart {
  constructor(id, series, fixedMax=null){ this.canvas=$(id); this.series=series; this.fixedMax=fixedMax; this.points=[]; addEventListener('resize',()=>this.draw()); }
  set(points){this.points=points;this.draw()}
  draw(){
    const c=this.canvas, dpr=devicePixelRatio||1, rect=c.getBoundingClientRect(); c.width=Math.max(1,rect.width*dpr); c.height=Math.max(1,rect.height*dpr);
    const x=c.getContext('2d'); x.scale(dpr,dpr); const w=rect.width,h=rect.height,p={l:45,r:12,t:10,b:26}; x.clearRect(0,0,w,h);
    x.strokeStyle='#163622'; x.lineWidth=1; x.fillStyle='#437352'; x.font='10px ui-monospace,monospace';
    let values=[]; for(const s of this.series) for(const point of this.points){const v=s.get(point);if(Number.isFinite(v))values.push(v)}
    let max=this.fixedMax||Math.max(1,...values)*1.12, min=0; if(this.fixedMax===null&&max<10)max=10;
    for(let i=0;i<=4;i++){const yy=p.t+(h-p.t-p.b)*i/4;x.beginPath();x.moveTo(p.l,yy);x.lineTo(w-p.r,yy);x.stroke();x.fillText((max-(max-min)*i/4).toFixed(max>100?0:1),3,yy+3)}
    if(this.points.length<2){x.fillStyle='#2f7f4b';x.fillText('WAITING FOR TELEMETRY...',p.l+20,h/2);return}
    const first=new Date(this.points[0].timestamp).getTime(), last=new Date(this.points.at(-1).timestamp).getTime(), span=Math.max(1,last-first);
    for(let i=0;i<=4;i++){const xx=p.l+(w-p.l-p.r)*i/4;const d=new Date(first+span*i/4);x.fillStyle='#437352';x.fillText(d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'}),Math.min(xx,w-55),h-6)}
    for(const s of this.series){x.strokeStyle=s.color;x.lineWidth=1.5;x.beginPath();let started=false;for(const point of this.points){const v=s.get(point);if(!Number.isFinite(v)){started=false;continue}const xx=p.l+(new Date(point.timestamp).getTime()-first)/span*(w-p.l-p.r),yy=p.t+(max-v)/(max-min)*(h-p.t-p.b);if(!started){x.moveTo(xx,yy);started=true}else{x.lineTo(xx,yy)}}x.stroke()}
    let lx=p.l;for(const s of this.series){x.fillStyle=s.color;x.fillRect(lx,p.t,9,2);x.fillText(s.name,lx+13,p.t+4);lx+=13+x.measureText(s.name).width+18}
  }
}

function initCharts(){
  state.charts=[
    new RetroChart('thermal-chart',[{name:'CPU',color:'#54ff88',get:p=>p.cpu_temp_c},{name:'NVME',color:'#ffc857',get:p=>p.nvme_temp_c}]),
    new RetroChart('cpu-chart',[{name:'USAGE',color:'#54ff88',get:p=>p.cpu_usage_pct},{name:'FREQ/100',color:'#58a6ff',get:p=>p.cpu_freq_mhz/100}]),
    new RetroChart('usage-chart',[{name:'MEM',color:'#c084fc',get:p=>p.memory_pct},{name:'DISK',color:'#ffc857',get:p=>p.disk_pct}],100),
		new RetroChart('fan-chart',[{name:'RPM',color:'#58a6ff',get:p=>p.fan_rpm}])];
	state.coreChart=new RetroChart('cores-chart',[],100);state.networkChart=new RetroChart('network-chart',[{name:'RX KiB/s',color:'#54ff88',get:p=>(p.network?.[state.networkInterface]?.rx_bytes_per_sec)/1024},{name:'TX KiB/s',color:'#58a6ff',get:p=>(p.network?.[state.networkInterface]?.tx_bytes_per_sec)/1024}]);state.charts.push(state.coreChart,state.networkChart);
}
const coreColors=['#54ff88','#58a6ff','#ffc857','#c084fc','#ff7b72','#39d0d8','#f0883e','#a5d6ff','#7ee787','#d2a8ff','#ffa198','#e3b341','#79c0ff','#56d364','#db61a2','#b1bac4'];
function redraw(){const count=Math.max(0,...state.samples.map(p=>(p.cpu_core_usage_pct||[]).length));state.coreChart.series=Array.from({length:count},(_,i)=>({name:`CPU${i}`,color:coreColors[i%coreColors.length],get:p=>(p.cpu_core_usage_pct||[])[i]}));state.charts.forEach(c=>c.set(state.samples))}
async function loadHistory(){
  const to=new Date(),from=new Date(to-ranges[state.range]);
  try{const r=await fetch(`/api/history?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}&max_points=1600`);if(!r.ok)throw Error(r.status);const d=await r.json();state.samples=d.samples||[];if(state.samples.length)updateCards(state.samples.at(-1));redraw()}catch(e){console.error(e)}
}
function connect(){
  const status=$('status'), events=new EventSource('/api/stream');
  events.onopen=()=>{status.textContent='● LIVE';status.className='status live'};
  events.onerror=()=>{status.textContent='● RECONNECTING';status.className='status offline'};
  events.onmessage=e=>{const p=JSON.parse(e.data);updateCards(p);const cutoff=Date.now()-ranges[state.range];state.samples.push(p);state.samples=state.samples.filter(v=>new Date(v.timestamp).getTime()>=cutoff);if(state.samples.length>2000)state.samples=state.samples.filter((_,i)=>i%2===0);redraw()};
}
document.querySelectorAll('[data-range]').forEach(b=>b.onclick=()=>{document.querySelectorAll('[data-range]').forEach(x=>x.classList.remove('active'));b.classList.add('active');state.range=b.dataset.range;loadHistory()});
initCharts();loadSystem().then(loadHistory);connect();setInterval(loadHistory,60000);
