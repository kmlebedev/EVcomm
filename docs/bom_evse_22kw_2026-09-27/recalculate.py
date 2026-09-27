#!/usr/bin/env python3
"""EVcomm BOM v3: allowed suppliers, explicit estimates and missing prices."""
import argparse
import json
import math
import re
from decimal import Decimal, ROUND_HALF_UP
from pathlib import Path
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parent
DOC = ROOT / 'nomenklatura_evse_22kw.md'
GROUPS = [
 ('site','S — общее оборудование площадки, один раз'),
 ('cabinet','C — один шкаф и единственный вводной счётчик'),
 ('cabinet_expansion','C30 — проверка/изменение корпуса при расширении'),
 ('branch','B — на каждую независимую однофазную ветвь'),
 ('oem','V — дополнительно на точку штатной EVSE пользователя'),
 ('type2','H — дополнительно на порт Type 2 с EKEPC2-S'),
 ('map','M — узел WB-MAP12E, один раз при его установке'),
 ('ct','CT — один датчик на каждую измеряемую ветвь'),
 ('interlock','X — блокировка на добавленную Type 2 ветвь, режим по очереди'),
 ('relay_extra','R — дополнительный MR6C сверх первых шести ветвей'),
 ('stand','T — стойка до двух точек подключения'),
 ('climate_c','Климат шкафа — опционально'),
 ('climate_point','Климат точки — опционально')]
CABLES = ('feed_m','power_m','aux_m','signal_m','permit_m','rs485_m','lan_m','route_m')
STATUSES = {'П':'direct','К':'card','У':'user','М':'conditional','О':'estimate','Н':'unknown'}

def dec(x):
    return Decimal(str(x))

def rub(x):
    if x is None: return '—'
    return f"{dec(x).quantize(Decimal('0.01'),rounding=ROUND_HALF_UP):,.2f}".replace(',',' ').replace('.',',')

def unit_price(data,x):
    p=data['parameters']
    if p.get('elektromir_club_card',False) and 'club_price' in x: return x['club_price']
    if not p.get('ozon_bank_discount',True) and 'alternative_price' in x: return x['alternative_price']
    return x['price']

def validate(data):
    p=data['parameters']
    ids=[x['id'] for x in data['items']]
    if len(ids)!=len(set(ids)): raise ValueError('Повтор ID позиции')
    groups={g for g,_ in GROUPS}|set(CABLES)
    for x in data['items']:
        if x['group'] not in groups or x['status'] not in STATUSES: raise ValueError('Неизвестная группа/статус: '+x['id'])
        values=[x['qty']]+[x[k] for k in ('price','budget_estimate','club_price','alternative_price') if x.get(k) is not None]
        if any(not dec(v).is_finite() or dec(v)<0 for v in values): raise ValueError('Неверная цена/количество: '+x['id'])
        host=(urlparse(x['url']).hostname or '').removeprefix('www.')
        if x['url'] and host not in data['allowed_purchase_domains']: raise ValueError('Магазин не разрешён: '+x['id'])
        if x['price'] is not None and (not x['url'] or x['status'] in ('О','Н')): raise ValueError('Цена без подтверждённого источника: '+x['id'])
        if x['price'] is None and x['status'] not in ('О','Н'): raise ValueError('Нет цены для статуса: '+x['id'])
        if x.get('budget_estimate') is not None and x['price'] is not None: raise ValueError('Цена и резерв задвоены: '+x['id'])
    if set(p['lengths'])!=set(CABLES): raise ValueError('Неверный набор длин')
    for x in [*p['lengths'].values(),p['module_distance_m'],p['route_per_stand_m'],p['rs485_distance_multiplier'],p['reserve_percent']]:
        if not dec(x).is_finite() or dec(x)<0: raise ValueError('Длины и резерв должны быть неотрицательными')
    if p['phase_mode'] not in ('exclusive','shared'): raise ValueError('phase_mode: exclusive или shared')
    if len(p['auxiliary_reserve_a'])!=3 or any(dec(x)<0 for x in p['auxiliary_reserve_a']): raise ValueError('Нужны три резерва тока')
    if any(dec(p[k])<=0 for k in ('voltage_phase','inlet_limit_kw','inlet_phase_limit_a','evse_current_a')): raise ValueError('Неверные параметры мощности')

def calculate(data,vertical=None,horizontal=None,include_site=True,map_from_start=None,include_stands=None):
    p=data['parameters']
    v=list(p['vertical_phases'] if vertical is None else vertical)
    h=list(p['horizontal_per_phase'] if horizontal is None else horizontal)
    if not v or len(set(v))!=len(v) or any(type(i) is not int or i not in (1,2,3) for i in v): raise ValueError('Штатные ветви: уникальные фазы 1,2,3, хотя бы одна')
    if len(h)!=3 or any(type(n) is not int or n<0 for n in h): raise ValueError('Горизонтальные порты: три целых неотрицательных числа')
    if any(h[i-1]>0 and i not in v for i in (1,2,3)): raise ValueError('Type 2 добавляется на уже задействованную фазу')
    nv,nh=len(v),sum(h)
    branches=nv+nh
    if branches>12: raise ValueError('Этот расчёт ограничен 12 ветвями одного шкафа / одного WB-MAP12E')
    early=p['map_from_start'] if map_from_start is None else map_from_start
    map_count=int(bool(nh or early))
    stands_enabled=p['include_stands'] if include_stands is None else include_stands
    stands=math.ceil(branches/2) if stands_enabled else 0
    relays=math.ceil(branches/6)
    units=dict(site=int(include_site),cabinet=1,cabinet_expansion=int(branches>1),branch=branches,oem=nv,type2=nh,map=map_count,
               ct=branches if map_count else 0,interlock=nh if p['phase_mode']=='exclusive' else 0,
               relay_extra=relays-1,stand=stands,climate_c=1,climate_point=branches)
    distance=dec(p['module_distance_m'])
    lengths=dict(feed_m=dec(0),power_m=branches*distance,aux_m=nh*distance,
                 signal_m=branches*distance,permit_m=nh*distance,
                 rs485_m=nh*distance*dec(p['rs485_distance_multiplier']),lan_m=dec(0),
                 route_m=math.ceil(branches/2)*dec(p['route_per_stand_m']))
    for k,value in p['lengths'].items(): lengths[k]+=dec(value)
    units.update(lengths)
    total={k:dec(0) for k in ('base','cables','climate',*STATUSES.values())}
    by_group={}; missing=[]; unbudgeted=[]; climate_missing=[]; climate_estimate=dec(0)
    for x in data['items']:
        quantity=dec(x['qty'])*dec(units[x['group']])
        price=unit_price(data,x)
        amount=dec(price or 0)*quantity
        by_group[x['group']]=by_group.get(x['group'],dec(0))+amount
        if x['group'].startswith('climate_'):
            total['climate']+=amount
            climate_estimate+=dec(x.get('budget_estimate',0))*quantity
            if quantity and price is None: climate_missing.append(x['id'])
            continue
        if price is None and quantity:
            missing.append(x['id'])
            if x.get('budget_estimate') is None: unbudgeted.append(x['id'])
        total['cables' if x['group'] in CABLES else 'base']+=amount
        total[STATUSES[x['status']]]+=amount
        total['estimate']+=dec(x.get('budget_estimate',0))*quantity
    total['materials']=total['base']+total['cables']
    total['reserve']=total['materials']*dec(p['reserve_percent'])/100
    total['reserved']=total['materials']+total['reserve']
    total['quoted']=total['materials']-total['conditional']
    total['partial_budget']=total['materials']+total['estimate']
    assert total['materials']==sum(total[k] for k in ('direct','card','user','conditional'))
    return dict(**total,units=units,lengths=lengths,groups=by_group,vertical=sorted(v),horizontal=h,relays=relays,
                missing=missing,unbudgeted=unbudgeted,climate_missing=climate_missing,climate_estimate=climate_estimate)

def standard_stages():
    return [('V1', [1], [0,0,0]),('V2',[1,2],[0,0,0]),('V3',[1,2,3],[0,0,0]),
            ('V3+H1',[1,2,3],[1,0,0]),('V3+H2',[1,2,3],[1,1,0]),('V3+H3',[1,2,3],[1,1,1])]

def replace_section(text,key,body):
    a,b=f'<!-- {key}:START -->',f'<!-- {key}:END -->'
    if text.count(a)!=1 or text.count(b)!=1: raise ValueError('Неверные маркеры '+key)
    return re.sub(re.escape(a)+r'.*?'+re.escape(b),lambda _:a+'\n\n'+body+'\n\n'+b,text,flags=re.S)

def render(data):
    p=data['parameters']
    stages=[(name,calculate(data,v,h)) for name,v,h in standard_stages()]
    first,full=stages[0][1],stages[-1][1]
    summary=(f"**Полная стоимость пока не подтверждена. Сумма строк с ценами магазинов/WB/пользователя: V1 — {rub(first['quoted'])} ₽; V3+H3 — {rub(full['quoted'])} ₽.** "
             f"Для V1 ещё {len(first['missing'])} позиций без цены товара; для V3+H3 — {len(full['missing'])}. "
             'Условные цены AliExpress «Комбо» и авторские резервы показаны отдельно ниже. Это не стоимость готового комплекта. '
             'Прежние итоги 188 742,69 ₽ и 410 099,69 ₽ заменены этой проверяемой разбивкой.')
    rows=['| Этап | Штатных / Type 2 | MAP / ТТ | Стойки | Цены товаров, ₽ | Комбо отдельно, ₽ | Авторский резерв без карточек, ₽ | Позиций без цены / из них без резерва | Прирост цен товаров, ₽ |',
          '|---|---:|---:|---:|---:|---:|---:|---:|---:|']
    prev=dec(0)
    for name,t in stages:
        u=t['units']
        rows.append(f"| {name} | {u['oem']} / {u['type2']} | {u['map']} / {u['ct']} | {u['stand']} | **{rub(t['quoted'])}** | {rub(t['conditional'])} | {rub(t['estimate'])} | {len(t['missing'])} / {len(t['unbudgeted'])} | {rub(t['quoted']-prev)} |")
        prev=t['quoted']
    rows+=['','«Позиций» — число разных строк ведомости с ненулевым количеством, не число физических деталей. Авторский резерв — сохранённые допущения для планирования; это не проверенные цены и не предложения магазинов. Прирост отражает только строки с ценами и не является полной стоимостью расширения.','',
           '| Вариант | Цены товаров, ₽ | Комбо отдельно, ₽ | Авторский резерв, ₽ | Позиций без цены |','|---|---:|---:|---:|---:|']
    variants=[('V1+H1',[1],[1,0,0],{}),('V2+H2',[1,2],[1,1,0],{}),('V1 с MAP сразу',[1],[0,0,0],{'map_from_start':True}),('V1 при наличии блока S',[1],[0,0,0],{'include_site':False})]
    for label,v,h,kw in variants:
        t=calculate(data,v,h,**kw)
        rows.append(f"| {label} | {rub(t['quoted'])} | {rub(t['conditional'])} | {rub(t['estimate'])} | {len(t['missing'])} |")
    rows += ['',f"Для V1 без цены и без авторского резерва: **{', '.join(first['unbudgeted']) or 'нет'}**. Для V3+H3: **{', '.join(full['unbudgeted']) or 'нет'}**.",
             f"Климат отдельно: авторский резерв V1 — {rub(first['climate_estimate'])} ₽, V3+H3 — {rub(full['climate_estimate'])} ₽; подтверждённых цен нет.",
             'Финансовый резерв '+str(p['reserve_percent'])+'% применяется только после заполнения стоимости; он не заменяет неизвестные цены.', '',
             'Ценовой профиль: '+('Ozon с указанными банками' if p.get('ozon_bank_discount',True) else 'Ozon с другими банками')+'; '+('ЭлектроМИР по клубной карте.' if p.get('elektromir_club_card',False) else 'ЭлектроМИР по рознице. Клубные цены указаны в примечаниях.'),
             'Цена B01 4 740 ₽ сохраняется по указанию пользователя: независимые банковские условия этой цены не подтверждены.']
    bom=[]; costs={}; budgets={}
    for group,title in GROUPS:
        bom += ['### '+title,'','| ID | Позиция | Кол-во на блок | Цена/ед., ₽ | Резерв/ед., ₽ | Источник | Условие |','|---|---|---:|---:|---:|---|---|']
        amount=dec(0); budget=dec(0)
        for x in data['items']:
            if x['group']!=group: continue
            price=unit_price(data,x)
            amount+=dec(price or 0)*dec(x['qty']); budget+=dec(x.get('budget_estimate',0))*dec(x['qty'])
            source=f"{x['status']} · [карточка]({x['url']})" if x['url'] else x['status']+' · карточка не подобрана'
            bom.append(f"| {x['id']} | {x['name']} | {x['qty']} {x['unit']} | {rub(price)} | {rub(x.get('budget_estimate'))} | {source} | {x['note']} |")
        costs[group]=amount; budgets[group]=budget
        bom += [f"| | **Сумма известных цен / отдельно резерв блока** | | **{rub(amount)}** | **{rub(budget)}** | | Не полная стоимость блока |",'']
    bom += ['### Внешние кабели и трассы','', '| ID | Кабель / трасса | Цена/м, ₽ | Резерв/м, ₽ | Источник | Условие |','|---|---|---:|---:|---|---|']
    for x in data['items']:
        if x['group'] in CABLES:
            source=f"{x['status']} · [карточка]({x['url']})" if x['url'] else x['status']+' · карточка не подобрана'
            bom.append(f"| {x['id']} | {x['name']} | {rub(unit_price(data,x))} | {rub(x.get('budget_estimate'))} | {source} | {x['note']} |")
    counts=['| Аппарат / узел | V1 | V2 | V3 | V3+H1 | V3+H2 | V3+H3 |','|---|---:|---:|---:|---:|---:|---:|']
    for label,get in [('Вводной трёхфазный Меркурий',lambda t:1),('Контакторы ветвей 1Ф',lambda t:t['units']['branch']),('Общий аварийный контактор K0',lambda t:1),('WB-MR6C',lambda t:t['relays']),('WB-MGE',lambda t:1),('WB-MAP12E',lambda t:t['units']['map']),('Трансформаторы тока MAP',lambda t:t['units']['ct']),('EKEPC2-S / Type 2 / RCMU / замок',lambda t:t['units']['type2']),('Дифавтоматы зарядных ветвей',lambda t:t['units']['branch']),('Штатные EVSE в закупке',lambda t:0)]:
        counts.append('| '+label+' | '+' | '.join(str(get(t)) for _,t in stages)+' |')
    formulas=['| Блок | Известные цены, ₽ | Отдельный авторский резерв, ₽ |','|---|---:|---:|']
    for group,title in GROUPS:
        if group.startswith('climate_'): continue
        formulas.append(f'| {title} | {rub(costs[group])} | {rub(budgets[group])} |')
    text=DOC.read_text(encoding='utf-8')
    for key,body in [('SUMMARY',summary),('TOTALS','\n'.join(rows)),('BOM','\n'.join(bom)),('COUNTS','\n'.join(counts)),('FORMULA','\n'.join(formulas))]:
        text=replace_section(text,key,body)
    DOC.write_text(text,encoding='utf-8')

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--vertical',type=int,choices=[1,2,3],help='Число штатных ветвей на L1, затем L2, L3')
    ap.add_argument('--horizontal',help='Число Type 2 по фазам, например 1,0,0 или 1,1,1')
    ap.add_argument('--map-from-start',action='store_true',default=None)
    ap.add_argument('--no-site',action='store_true')
    ap.add_argument('--no-stands',action='store_true')
    ap.add_argument('--print-only',action='store_true')
    args=ap.parse_args()
    try:
        data=json.loads((ROOT/'prices.json').read_text(encoding='utf-8')); validate(data)
        v=list(range(1,args.vertical+1)) if args.vertical is not None else None
        h=[int(x.strip()) for x in args.horizontal.split(',')] if args.horizontal is not None else None
        t=calculate(data,v,h,not args.no_site,args.map_from_start,False if args.no_stands else None)
    except (ValueError,KeyError) as e: ap.error(str(e))
    if not args.print_only: render(data)
    u=t['units']
    print(f"Штатные ветви на фазах {t['vertical']}; Type 2 по фазам {t['horizontal']}; режим {data['parameters']['phase_mode']}")
    print(f"Ветвей {u['branch']}; вводных счётчиков 1; WB-MAP12E {u['map']}; ТТ {u['ct']}; MR6C {t['relays']}; стоек {u['stand']}")
    for key,label in [('quoted','Сумма цен товаров'),('conditional','Комбо отдельно'),('estimate','Авторские резервы отдельно'),('climate_estimate','Резерв климата отдельно')]: print(f'{label}: {rub(t[key])} ₽')
    print(f"Без цены: {len(t['missing'])} позиций; без цены и резерва: {', '.join(t['unbudgeted']) or 'нет'}. Полная стоимость не подтверждена.")
    p=data['parameters']; voltage=dec(p['voltage_phase']); current=dec(p['evse_current_a'])
    if any(current+dec(p['auxiliary_reserve_a'][phase-1])>dec(p['inlet_phase_limit_a']) for phase in t['vertical']) or voltage*(current*len(t['vertical'])+sum(map(dec,p['auxiliary_reserve_a'])))/1000>dec(p['inlet_limit_kw']):
        print('Уставка 32 А с резервом собственных нужд требует уменьшения/уточнения. Номинал аппаратуры не равен разрешённому току сессии.')
    if u['type2']: print('Добавленные ветви делят тот же фазный лимит; их мощности не складываются как независимые вводы.')
    if u['branch']>6: print('Более 6 ветвей: рассчитан дополнительный MR6C; нужна новая проверка корпуса, клемм, блокировок и RS-485.')
    print('Штатные EVSE, монтаж и доставка не включены. Стандартные таблицы MD обновляются отдельно от выбранного сценария CLI.')

if __name__=='__main__': main()
