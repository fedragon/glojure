package compiler

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/glojurelang/glojure/ast"
	"github.com/glojurelang/glojure/value"
)

var (
	ctxExpr      = kw("ctx/expr")
	ctxReturn    = kw("ctx/return")
	ctxStatement = kw("ctx/statement")

	symCatch   = value.NewSymbol("catch")
	symFinally = value.NewSymbol("finally")
)

type (
	Env value.IPersistentMap

	Analyzer struct {
		Macroexpand1 func(form interface{}) (interface{}, error)
		CreateVar    func(sym *value.Symbol, env Env) (interface{}, error)
		IsVar        func(v interface{}) bool

		Gensym func(prefix string) *value.Symbol

		GlobalEnv *value.Atom
	}
)

// Analyze performs semantic analysis on the given s-expression,
// returning an AST.
func (a *Analyzer) Analyze(form interface{}, env Env) (ast.Node, error) {
	return a.analyzeForm(form, ctxEnv(env, ctxExpr).Assoc(kw("top-level"), true).(Env))
}

func (a *Analyzer) analyzeForm(form interface{}, env Env) (n ast.Node, err error) {
	switch v := form.(type) {
	case *value.Symbol:
		return a.analyzeSymbol(v, env)
	case value.IPersistentVector:
		return a.analyzeVector(v, env)
	case value.IPersistentMap:
		return a.analyzeMap(v, env)
	case value.IPersistentSet:
		return a.analyzeSet(v, env)
	case value.ISeq:
		return a.analyzeSeq(v, env)
	default:
		return a.analyzeConst(v, env)
	}
}

// analyzeSymbol performs semantic analysis on the given symbol,
// returning an AST.
func (a *Analyzer) analyzeSymbol(form *value.Symbol, env Env) (ast.Node, error) {
	mform, err := a.Macroexpand1(form)
	if err != nil {
		return nil, err
	}
	if !value.Equal(form, mform) {
		n, err := a.analyzeForm(mform, env)
		if err != nil {
			return nil, err
		}
		return withRawForm(n, form), nil
	}

	var n ast.Node
	if localBinding := value.Get(value.Get(env, kw("locals")), form); localBinding != nil {
		mutable := value.Get(localBinding, kw("mutable"))
		children := value.Get(localBinding, kw("children"))
		n = merge(value.Dissoc(localBinding, kw("init")), value.NewMap(
			kw("op"), kw("local"),
			kw("assignable?"), mutable != nil && mutable != false,
			kw("children"), value.NewVectorFromCollection(remove(kw("init"), children)),
		))
	} else {
		v := a.resolveSym(form, env)
		vr, ok := v.(*value.Var)
		if ok {
			m := vr.Meta()
			n = value.NewMap(
				kw("op"), kw("var"),
				// kw("assignable?"), dynamicVar(vr, m), // TODO
				kw("var"), vr,
				kw("meta"), m,
			)
		} else {
			maybeClass := form.Namespace()
			if maybeClass != "" {
				n = value.NewMap(
					kw("op"), kw("maybe-host-form"), // TODO: define this for Go interop
					kw("class"), maybeClass,
					kw("field"), value.NewSymbol(form.Name()),
				)
			} else {
				n = value.NewMap(
					kw("op"), kw("maybe-class"),
					kw("class"), mform,
				)
			}
		}
	}

	return merge(n, value.NewMap(
		kw("env"), env,
		kw("form"), mform,
	)), nil
}

// analyzeVector performs semantic analysis on the given vector,
// returning an AST.
func (a *Analyzer) analyzeVector(form value.IPersistentVector, env Env) (ast.Node, error) {
	n := ast.MakeNode(kw("vector"), form)
	var items []interface{}
	for i := 0; i < form.Count(); i++ {
		// TODO: pass an "items-env" with an expr context
		nn, err := a.analyzeForm(value.MustNth(form, i), env)
		if err != nil {
			return nil, err
		}

		items = append(items, nn)
	}
	n = n.Assoc(kw("items"), vec(items...))
	return n.Assoc(kw("children"), vec(kw("items"))), nil
}

// analyzeMap performs semantic analysis on the given map,
// returning an AST.
func (a *Analyzer) analyzeMap(v value.IPersistentMap, env Env) (ast.Node, error) {
	n := ast.MakeNode(kw("map"), v)
	var keys []interface{}
	var vals []interface{}
	for seq := value.Seq(v); seq != nil; seq = seq.Next() {
		// TODO: pass a "kv-env" with an expr context

		entry := seq.First().(*value.MapEntry)
		keyNode, err := a.analyzeForm(entry.Key(), env)
		if err != nil {
			return nil, err
		}
		valNode, err := a.analyzeForm(entry.Val(), env)
		if err != nil {
			return nil, err
		}
		keys = append(keys, keyNode)
		vals = append(vals, valNode)
	}
	n = n.Assoc(kw("keys"), vec(keys...)).
		Assoc(kw("vals"), vec(vals...))
	return n.Assoc(kw("children"), vec(kw("keys"), kw("vals"))), nil
}

// analyzeSet performs semantic analysis on the given set,
// returning an AST.
func (a *Analyzer) analyzeSet(v value.IPersistentSet, env Env) (ast.Node, error) {
	n := ast.MakeNode(kw("set"), v)
	items := make([]interface{}, 0, v.Count())
	for seq := value.Seq(v); seq != nil; seq = seq.Next() {
		// TODO: pass an "items-env" with an expr context
		item, err := a.analyzeForm(seq.First(), env)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	n = n.Assoc(kw("items"), vec(items...))
	return n.Assoc(kw("children"), vec(kw("items"))), nil
}

// analyzeSeq performs semantic analysis on the given sequence,
// returning an AST.
func (a *Analyzer) analyzeSeq(form value.ISeq, env Env) (ast.Node, error) {
	if value.Seq(form) == nil {
		return a.analyzeConst(form, env)
	}

	op := form.First()
	if op == nil {
		return nil, exInfo("can't call nil", nil) // TODO: include form and source info
	}
	mform, err := a.Macroexpand1(form)
	if err != nil {
		return nil, err
	}

	if value.Equal(form, mform) {
		return a.parse(form, env)
	}
	n, err := a.analyzeForm(mform, env)
	if err != nil {
		return nil, err
	}

	// TODO: add resolved-op to meta
	return withRawForm(n, form), nil
}

// analyzeConst performs semantic analysis on the given constant
// expression,
func (a *Analyzer) analyzeConst(v interface{}, env Env) (ast.Node, error) {
	n := ast.MakeNode(kw("const"), v)
	n = n.Assoc(kw("type"), classifyType(v)).
		Assoc(kw("val"), v).
		Assoc(kw("literal?"), true)

	if im, ok := v.(value.IMeta); ok {
		meta := im.Meta()
		if meta != nil {
			mn, err := a.analyzeConst(meta, env)
			if err != nil {
				return nil, err
			}
			n = n.Assoc(kw("meta"), mn).
				Assoc(kw("children"), vec(kw("meta")))
		}
	}
	return n, nil
}

func (a *Analyzer) analyzeBody(body interface{}, env Env) (ast.Node, error) {
	n, err := a.parse(value.NewCons(value.NewSymbol("do"), body), env)
	if err != nil {
		return nil, err
	}
	return n.Assoc(kw("body?"), true), nil
}

// (defn analyze-let
//
//	[[op bindings & body :as form] {:keys [context loop-id] :as env}]
//	(validate-bindings form env)
//	(let [loop? (= 'loop* op)]
//	  (loop [bindings bindings
//	         env (ctx env :ctx/expr)
//	         binds []]
//	    (if-let [[name init & bindings] (seq bindings)]
//	      (if (not (valid-binding-symbol? name))
//	        (throw (ex-info (str "Bad binding form: " name)
//	                        (merge {:form form
//	                                :sym  name}
//	                               (-source-info form env))))
//	        (let [init-expr (analyze-form init env)
//	              bind-expr {:op       :binding
//	                         :env      env
//	                         :name     name
//	                         :init     init-expr
//	                         :form     name
//	                         :local    (if loop? :loop :let)
//	                         :children [:init]}]
//	          (recur bindings
//	                 (assoc-in env [:locals name] (dissoc-env bind-expr))
//	                 (conj binds bind-expr))))
//	      (let [body-env (assoc env :context (if loop? :ctx/return context))
//	            body (analyze-body body (merge body-env
//	                                           (when loop?
//	                                             {:loop-id     loop-id
//	                                              :loop-locals (count binds)})))]
//	        {:body     body
//	         :bindings binds
//	         :children [:bindings :body]})))))
func (a *Analyzer) analyzeLet(form interface{}, env Env) (ast.Node, error) {
	if value.Count(form) < 2 {
		return nil, exInfo("let requires a binding vector and a body", nil)
	}
	op := value.MustNth(form, 0)
	bindings := value.MustNth(form, 1)
	body := value.Rest(value.Rest(form))

	ctx := value.Get(env, kw("context"))
	loopID := value.Get(env, kw("loop-id"))

	if err := a.validateBindings(form, env); err != nil {
		return nil, err
	}

	isLoop := value.Equal(op, value.NewSymbol("loop*"))
	localKW := kw("let")
	if isLoop {
		localKW = kw("loop")
	}
	env = ctxEnv(env, kw("ctx/expr"))
	binds := vec()
	for {
		bindingsSeq := value.Seq(bindings)
		if bindingsSeq == nil {
			break
		}
		name := bindingsSeq.First()
		init := second(bindingsSeq)
		bindings = value.Rest(value.Rest(bindingsSeq))
		if !isValidBindingSymbol(name) {
			return nil, exInfo("bad binding form: "+value.ToString(name), nil)
		}
		initExpr, err := a.analyzeForm(init, env)
		if err != nil {
			return nil, err
		}
		bindExpr := merge(ast.MakeNode(kw("binding"), name),
			value.NewMap(
				kw("env"), env,
				kw("name"), name,
				kw("init"), initExpr,
				kw("local"), localKW,
				kw("children"), vec(kw("init")),
			),
		)
		env = assocIn(env, vec(kw("locals"), name), dissocEnv(bindExpr)).(Env)
		binds = value.Conj(binds, bindExpr).(*value.Vector)
	}
	if isLoop {
		ctx = kw("ctx/return")
	}
	bodyEnv := value.Assoc(env, kw("context"), ctx).(Env)
	if isLoop {
		bodyEnv = merge(bodyEnv, value.NewMap(
			kw("loop-id"), loopID,
			kw("loop-locals"), value.Count(binds),
		)).(Env)
	}
	body, err := a.analyzeBody(body, bodyEnv)
	if err != nil {
		return nil, err
	}
	return value.NewMap(
		kw("body"), body,
		kw("bindings"), binds,
		kw("children"), vec(kw("bindings"), kw("body")),
	), nil
}

////////////////////////////////////////////////////////////////////////////////
// Parse

func (a *Analyzer) parse(form interface{}, env Env) (ast.Node, error) {
	op := value.First(form)
	opSym, ok := op.(*value.Symbol)
	if !ok {
		return a.parseInvoke(form, env)
	}
	switch opSym.Name() {
	case "do":
		return a.parseDo(form, env)
	case "if":
		return a.parseIf(form, env)
	case "new":
		return a.parseNew(form, env)
	case "quote":
		return a.parseQuote(form, env)
	case "set!":
		return a.parseSetBang(form, env)
	case "try":
		return a.parseTry(form, env)
	case "throw":
		return a.parseThrow(form, env)
	case "def":
		return a.parseDef(form, env)
	case ".":
		return a.parseDot(form, env)
	case "let*":
		return a.parseLetStar(form, env)
	case "letfn*":
		return a.parseLetfnStar(form, env)
	case "loop*":
		return a.parseLoopStar(form, env)
	case "recur":
		return a.parseRecur(form, env)
	case "fn*":
		return a.parseFnStar(form, env)
	case "var":
		return a.parseVar(form, env)
	case "case*":
		return a.parseCaseStar(form, env)
	}

	return a.parseInvoke(form, env)
}

func (a *Analyzer) parseInvoke(form interface{}, env Env) (ast.Node, error) {
	f := value.First(form)
	args := value.Rest(form)
	fenv := ctxEnv(env, ctxExpr)
	fnExpr, err := a.analyzeForm(f, fenv)
	if err != nil {
		return nil, err
	}
	argsExprs := make([]interface{}, 0, value.Count(args))
	for seq := value.Seq(args); seq != nil; seq = seq.Next() {
		n, err := a.analyzeForm(seq.First(), fenv)
		if err != nil {
			return nil, err
		}
		argsExprs = append(argsExprs, n)
	}
	var meta value.IPersistentMap
	if m, ok := form.(value.IMeta); ok && value.Seq(m.Meta()) != nil {
		meta = value.NewMap(kw("meta"), m.Meta())
	}
	return merge(ast.MakeNode(kw("invoke"), form), value.NewMap(
		kw("fn"), fnExpr,
		kw("args"), vec(argsExprs...)),
		meta,
		value.NewMap(kw("children"), vec(kw("fn"), kw("args")))), nil
}

// (defn parse-do
//
//	[[_ & exprs :as form] env]
//	(let [statements-env (ctx env :ctx/statement)
//	      [statements ret] (loop [statements [] [e & exprs] exprs]
//	                         (if (seq exprs)
//	                           (recur (conj statements e) exprs)
//	                           [statements e]))
//	      statements (mapv (analyze-in-env statements-env) statements)
//	      ret (analyze-form ret env)]
//	  {:op         :do
//	   :env        env
//	   :form       form
//	   :statements statements
//	   :ret        ret
//	   :children   [:statements :ret]}))
func (a *Analyzer) parseDo(form interface{}, env Env) (ast.Node, error) {
	exprs := value.Rest(form)
	statementsEnv := ctxEnv(env, ctxStatement)
	var statementForms []interface{}
	retForm := value.First(exprs)
	for exprs := value.Seq(value.Rest(exprs)); exprs != nil; exprs = exprs.Next() {
		statementForms = append(statementForms, retForm)
		retForm = exprs.First()
	}
	statements := make([]interface{}, len(statementForms))
	for i, form := range statementForms {
		s, err := a.analyzeForm(form, statementsEnv)
		if err != nil {
			return nil, err
		}
		statements[i] = s
	}

	ret, err := a.analyzeForm(retForm, env)
	if err != nil {
		return nil, err
	}

	return merge(ast.MakeNode(kw("do"), form),
		value.NewMap(
			kw("env"), env,
			kw("statements"), vec(statements...),
			kw("ret"), ret,
			kw("children"), vec(kw("statements"), kw("ret")),
		)), nil
}

// (defn parse-if
//
//	[[_ test then else :as form] env]
//	(let [formc (count form)]
//	  (when-not (or (= formc 3) (= formc 4))
//	    (throw (ex-info (str "Wrong number of args to if, had: " (dec (count form)))
//	                    (merge {:form form}
//	                           (-source-info form env))))))
//	(let [test-expr (analyze-form test (ctx env :ctx/expr))
//	      then-expr (analyze-form then env)
//	      else-expr (analyze-form else env)]
//	  {:op       :if
//	   :form     form
//	   :env      env
//	   :test     test-expr
//	   :then     then-expr
//	   :else     else-expr
//	   :children [:test :then :else]}))
func (a *Analyzer) parseIf(form interface{}, env Env) (ast.Node, error) {
	formc := value.Count(form)
	if formc != 3 && formc != 4 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to if, had: %d", formc-1), nil)
	}

	test := value.MustNth(form, 1)
	then := value.MustNth(form, 2)
	var els interface{}
	if formc == 4 {
		els = value.MustNth(form, 3)
	}
	testExpr, err := a.analyzeForm(test, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	thenExpr, err := a.analyzeForm(then, env)
	if err != nil {
		return nil, err
	}
	elseExpr, err := a.analyzeForm(els, env)
	if err != nil {
		return nil, err
	}
	return merge(ast.MakeNode(kw("if"), form),
		value.NewMap(
			kw("env"), env,
			kw("test"), testExpr,
			kw("then"), thenExpr,
			kw("else"), elseExpr,
			kw("children"), vec(kw("test"), kw("then"), kw("else")),
		)), nil
}

// (defn parse-new
//
//	[[_ class & args :as form] env]
//	(when-not (>= (count form) 2)
//	  (throw (ex-info (str "Wrong number of args to new, had: " (dec (count form)))
//	                  (merge {:form form}
//	                         (-source-info form env)))))
//	(let [args-env (ctx env :ctx/expr)
//	      args (mapv (analyze-in-env args-env) args)]
//	  {:op          :new
//	   :env         env
//	   :form        form
//	   :class       (analyze-form class (assoc env :locals {})) ;; avoid shadowing
//	   :args        args
//	   :children    [:class :args]}))
func (a *Analyzer) parseNew(form interface{}, env Env) (ast.Node, error) {
	if value.Count(form) < 2 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to new, had: %d", value.Count(form)-1), nil)
	}

	class := value.MustNth(form, 1)
	args := value.Rest(value.Rest(form))
	argsEnv := ctxEnv(env, ctxExpr)
	argsExprs := vec()
	for seq := value.Seq(args); seq != nil; seq = seq.Next() {
		arg, err := a.analyzeForm(seq.First(), argsEnv)
		if err != nil {
			return nil, err
		}
		argsExprs = argsExprs.Conj(arg).(*value.Vector)
	}
	classExpr, err := a.analyzeForm(class, env.Assoc(kw("locals"), value.NewMap()).(Env))
	if err != nil {
		return nil, err
	}
	return value.NewMap(
		kw("op"), kw("new"),
		kw("env"), env,
		kw("form"), form,
		kw("class"), classExpr,
		kw("args"), argsExprs,
		kw("children"), vec(kw("class"), kw("args")),
	), nil
}

func (a *Analyzer) parseQuote(form interface{}, env Env) (ast.Node, error) {
	expr := second(form)
	if value.Count(form) != 2 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to quote, had: %v", value.Count(form)-1), nil)
	}
	cnst, err := a.analyzeConst(expr, env)
	if err != nil {
		return nil, err
	}
	n := ast.MakeNode(kw("quote"), form)
	return merge(n, value.NewMap(
		kw("expr"), cnst,
		kw("env"), env,
		kw("literal?"), true,
		kw("children"), vec(kw("expr")),
	)), nil
}

// (defn parse-set!
//
//	[[_ target val :as form] env]
//	(when-not (= 3 (count form))
//	  (throw (ex-info (str "Wrong number of args to set!, had: " (dec (count form)))
//	                  (merge {:form form}
//	                         (-source-info form env)))))
//	(let [target (analyze-form target (ctx env :ctx/expr))
//	      val (analyze-form val (ctx env :ctx/expr))]
//	  {:op       :set!
//	   :env      env
//	   :form     form
//	   :target   target
//	   :val      val
//	   :children [:target :val]}))
func (a *Analyzer) parseSetBang(form interface{}, env Env) (ast.Node, error) {
	if value.Count(form) != 3 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to set!, had: %d", value.Count(form)-1), nil)
	}
	target := value.MustNth(form, 1)
	val := value.MustNth(form, 2)

	targetExpr, err := a.analyzeForm(target, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	valExpr, err := a.analyzeForm(val, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	return merge(ast.MakeNode(kw("set!"), form),
		value.NewMap(
			kw("env"), env,
			kw("target"), targetExpr,
			kw("val"), valExpr,
			kw("children"), vec(kw("target"), kw("val")),
		)), nil
}

// (defn parse-try
//
//	[[_ & body :as form] env]
//	(let [catch? (every-pred seq? #(= (first %) 'catch))
//	      finally? (every-pred seq? #(= (first %) 'finally))
//	      [body tail'] (split-with' (complement (some-fn catch? finally?)) body)
//	      [cblocks tail] (split-with' catch? tail')
//	      [[fblock & fbs :as fblocks] tail] (split-with' finally? tail)]
//	  (when-not (empty? tail)
//	    (throw (ex-info "Only catch or finally clause can follow catch in try expression"
//	                    (merge {:expr tail
//	                            :form form}
//	                           (-source-info form env)))))
//	  (when-not (empty? fbs)
//	    (throw (ex-info "Only one finally clause allowed in try expression"
//	                    (merge {:expr fblocks
//	                            :form form}
//	                           (-source-info form env)))))
//	  (let [env' (assoc env :in-try true)
//	        body (analyze-body body env')
//	        cenv (ctx env' :ctx/expr)
//	        cblocks (mapv #(parse-catch % cenv) cblocks)
//	        fblock (when-not (empty? fblock)
//	                 (analyze-body (rest fblock) (ctx env :ctx/statement)))]
//	    (merge {:op      :try
//	            :env     env
//	            :form    form
//	            :body    body
//	            :catches cblocks}
//	           (when fblock
//	             {:finally fblock})
//	           {:children (into [:body :catches]
//	                            (when fblock [:finally]))}))))
func (a *Analyzer) parseTry(form interface{}, env Env) (ast.Node, error) {
	catch := func(form interface{}) bool {
		return value.IsSeq(form) && value.Equal(symCatch, value.First(form))
	}
	finally := func(form interface{}) bool {
		return value.IsSeq(form) && value.Equal(symFinally, value.First(form))
	}
	body, tail := splitWith(func(form interface{}) bool {
		return !catch(form) && !finally(form)
	}, value.Rest(form))
	cblocks, tail := splitWith(catch, tail)
	fblocks, tail := splitWith(finally, tail)
	if value.Count(tail) != 0 {
		return nil, exInfo("only catch or finally clause can follow catch in try expression", nil)
	}
	if value.Count(fblocks) > 1 {
		return nil, exInfo("only one finally clause allowed in try expression", nil)
	}
	env = env.Assoc(kw("in-try"), true).(Env)
	bodyExpr, err := a.analyzeBody(body, env)
	if err != nil {
		return nil, err
	}
	cenv := ctxEnv(env, ctxExpr)
	var cblocksExpr value.Conjer = vec()
	for cblockSeq := value.Seq(cblocks); cblockSeq != nil; cblockSeq = value.Next(cblockSeq) {
		cblock := value.First(cblockSeq)
		cblockExpr, err := a.parseCatch(cblock, cenv)
		if err != nil {
			return nil, err
		}
		cblocksExpr = cblocksExpr.Conj(cblockExpr)
	}
	var fblockExpr interface{}
	if value.Count(fblocks) > 0 {
		fblock := value.First(fblocks)
		fblockExpr, err = a.analyzeBody(value.Rest(fblock), ctxEnv(env, ctxStatement))
		if err != nil {
			return nil, err
		}
	}
	children := []interface{}{kw("body"), kw("catches")}
	if fblockExpr != nil {
		children = append(children, kw("finally"))
	}
	return merge(ast.MakeNode(kw("try"), form),
		value.NewMap(
			kw("env"), env,
			kw("body"), bodyExpr,
			kw("catches"), cblocksExpr,
			kw("finally"), fblockExpr,
			kw("children"), vec(children...),
		)), nil
}

// (defn parse-catch
//
//	[[_ etype ename & body :as form] env]
//	(when-not (valid-binding-symbol? ename)
//	  (throw (ex-info (str "Bad binding form: " ename)
//	                  (merge {:sym ename
//	                          :form form}
//	                         (-source-info form env)))))
//	(let [env (dissoc env :in-try)
//	      local {:op    :binding
//	             :env   env
//	             :form  ename
//	             :name  ename
//	             :local :catch}]
//	  {:op          :catch
//	   :class       (analyze-form etype (assoc env :locals {}))
//	   :local       local
//	   :env         env
//	   :form        form
//	   :body        (analyze-body body (assoc-in env [:locals ename] (dissoc-env local)))
//	   :children    [:class :local :body]}))
func (a *Analyzer) parseCatch(form interface{}, env Env) (ast.Node, error) {
	etype := value.First(value.Rest(form))
	ename := value.First(value.Rest(value.Rest(form)))
	if !isValidBindingSymbol(ename) {
		return nil, exInfo("bad binding form: "+value.ToString(ename), nil)
	}
	env = value.Dissoc(env, kw("in-try")).(Env)
	local := ast.MakeNode(kw("binding"), ename)
	local = merge(local,
		value.NewMap(
			kw("env"), env,
			kw("name"), ename,
			kw("local"), kw("catch"),
		))
	body, err := a.analyzeBody(value.Rest(value.Rest(value.Rest(form))), env.Assoc(kw("locals"), value.NewMap()).(Env))
	if err != nil {
		return nil, err
	}
	class, err := a.analyzeForm(etype, env.Assoc(kw("locals"), value.NewMap()).(Env))
	if err != nil {
		return nil, err
	}
	return merge(ast.MakeNode(kw("catch"), form),
		value.NewMap(
			kw("env"), env,
			kw("class"), class,
			kw("local"), local,
			kw("body"), body,
			kw("children"), vec(kw("class"), kw("local"), kw("body")),
		)), nil
}

// (defn parse-throw
//
//	[[_ throw :as form] env]
//	(when-not (= 2 (count form))
//	  (throw (ex-info (str "Wrong number of args to throw, had: " (dec (count form)))
//	                  (merge {:form form}
//	                         (-source-info form env)))))
//	{:op        :throw
//	 :env       env
//	 :form      form
//	 :exception (analyze-form throw (ctx env :ctx/expr))
//	 :children  [:exception]})
func (a *Analyzer) parseThrow(form interface{}, env Env) (ast.Node, error) {
	throw := second(form)
	if value.Count(form) != 2 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to throw, had: %d", value.Count(form)-1), nil)
	}
	exception, err := a.analyzeForm(throw, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	return value.NewMap(
		kw("op"), kw("throw"),
		kw("env"), env,
		kw("form"), form,
		kw("exception"), exception,
		kw("children"), vec(kw("exception")),
	), nil
}

func (a *Analyzer) parseDef(form interface{}, env Env) (ast.Node, error) {
	symForm := value.First(value.Rest(form))
	expr := value.Rest(value.Rest(form))

	sym, ok := symForm.(*value.Symbol)
	if !ok {
		return nil, exInfo(fmt.Sprintf("first argument to def must be a symbol, got %T", symForm), nil)
	}

	if sym.Namespace() != "" && sym.Namespace() != value.Get(env, kw("ns")).(*value.Symbol).Name() {
		return nil, exInfo("can't def namespace-qualified symbol", nil)
	}

	var args value.Associative
	var doc interface{}
	switch value.Count(expr) {
	case 0:
		// no-op
	case 1:
		args = value.Assoc(args, kw("init"), value.First(expr))
	case 2:
		doc = value.First(expr)
		init := value.First(value.Rest(expr))
		args = value.Assoc(args, kw("init"), init).
			Assoc(kw("doc"), doc)
	default:
		return nil, exInfo("invalid def", nil)
	}
	if doc == nil {
		doc = value.Get(sym.Meta(), kw("doc"))
	} else if _, ok := doc.(string); !ok {
		return nil, exInfo("doc must be a string", nil)
	}
	arglists := value.Get(sym.Meta(), kw("arglists"))
	if arglists != nil {
		arglists = second(arglists)
	}
	sym = value.NewSymbol(sym.Name()).WithMeta(
		merge(value.NewMap(), // hack to make sure we get a non-nil map
			sym.Meta(),
			mapWhen(kw("arglists"), arglists),
			mapWhen(kw("doc"), doc),
			// TODO: source info
		).(value.IPersistentMap)).(*value.Symbol)

	vr, err := a.CreateVar(sym, env)
	if err != nil {
		return nil, err
	}
	// TODO: make sure we have an environment
	// TODO: mutate the environment to add the var to the namespace

	meta := sym.Meta()
	if arglists != nil {
		meta = merge(meta, value.NewMap(kw("arglists"), value.NewList(value.NewSymbol("quote"), arglists))).(value.IPersistentMap)
	}
	var metaExpr ast.Node
	if meta != nil {
		var err error
		metaExpr, err = a.analyzeForm(meta, ctxEnv(env, ctxExpr))
		if err != nil {
			return nil, err
		}
	}

	var hasInit bool
	if init := value.Get(args, kw("init")); init != nil {
		initNode, err := a.analyzeForm(init, ctxEnv(env, ctxExpr))
		if err != nil {
			return nil, err
		}
		args = args.Assoc(kw("init"), initNode)
		hasInit = true
	}

	children := vec()
	if meta != nil {
		children = children.Conj(kw("meta")).(*value.Vector)
	}
	if hasInit {
		children = children.Conj(kw("init")).(*value.Vector)
	}
	var childrenMap, metaMap value.IPersistentMap
	if children.Count() > 0 {
		childrenMap = value.NewMap(kw("children"), children)
	}
	if meta != nil {
		metaMap = value.NewMap(kw("meta"), metaExpr)
	}

	n := ast.MakeNode(kw("def"), form)
	return merge(n, value.NewMap(
		kw("env"), env,
		kw("name"), sym,
		kw("var"), vr,
	),
		metaMap,
		args,
		childrenMap), nil
}

// (defn parse-dot
//
//	[[_ target & [m-or-f & args] :as form] env]
//	(when-not (>= (count form) 3)
//	  (throw (ex-info (str "Wrong number of args to ., had: " (dec (count form)))
//	                  (merge {:form form}
//	                         (-source-info form env)))))
//	(let [[m-or-f field?] (if (and (symbol? m-or-f)
//	                               (= \- (first (name m-or-f))))
//	                        [(-> m-or-f name (subs 1) symbol) true]
//	                        [(if args (cons m-or-f args) m-or-f) false])
//	      target-expr (analyze-form target (ctx env :ctx/expr))
//	      call? (and (not field?) (seq? m-or-f))]
//	  (when (and call? (not (symbol? (first m-or-f))))
//	    (throw (ex-info (str "Method name must be a symbol, had: " (class (first m-or-f)))
//	                    (merge {:form   form
//	                            :method m-or-f}
//	                           (-source-info form env)))))
//	  (merge {:form   form
//	          :env    env
//	          :target target-expr}
//	         (cond
//	          call?
//	          {:op       :host-call
//	           :method   (symbol (name (first m-or-f)))
//	           :args     (mapv (analyze-in-env (ctx env :ctx/expr)) (next m-or-f))
//	           :children [:target :args]}
//	          field?
//	          {:op          :host-field
//	           :assignable? true
//	           :field       (symbol (name m-or-f))
//	           :children    [:target]}
//	          :else
//	          {:op          :host-interop ;; either field access or no-args method call
//	           :assignable? true
//	           :m-or-f      (symbol (name m-or-f))
//	           :children    [:target]}))))
func (a *Analyzer) parseDot(form interface{}, env Env) (ast.Node, error) {
	if value.Count(form) < 3 {
		return nil, exInfo("wrong number of args to ., had: %d", value.Count(form)-1)
	}
	target := second(form)
	mOrF := value.MustNth(form, 2)
	args := value.Rest(value.Rest(value.Rest(form)))
	isField := false
	if sym, ok := mOrF.(*value.Symbol); ok && len(sym.Name()) > 0 && sym.Name()[0] == '-' {
		mOrF = value.NewSymbol(sym.Name()[1:])
		isField = true
	} else if value.Count(args) != 0 {
		mOrF = value.NewCons(mOrF, args)
	}
	targetExpr, err := a.analyzeForm(target, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	call := false
	if _, ok := mOrF.(value.ISeq); ok && !isField {
		call = true
	}
	if call {
		if _, ok := value.First(mOrF).(*value.Symbol); !ok {
			return nil, exInfo(fmt.Sprintf("method name must be a symbol, had: %T", value.First(mOrF)), nil)
		}
	}

	n := value.NewMap(kw("form"), form, kw("env"), env, kw("target"), targetExpr)
	switch {
	case call:
		var argNodes []interface{}
		for seq := value.Seq(value.Rest(mOrF)); seq != nil; seq = seq.Next() {
			arg := value.First(seq)
			argNode, err := a.analyzeForm(arg, ctxEnv(env, ctxExpr))
			if err != nil {
				return nil, err
			}
			argNodes = append(argNodes, argNode)
		}
		return merge(n, value.NewMap(
			kw("op"), kw("host-call"),
			kw("method"), value.NewSymbol(value.First(mOrF).(*value.Symbol).Name()),
			kw("args"), vec(argNodes...),
			kw("children"), vec(kw("target"), kw("args")),
		)), nil
	case isField:
		return merge(n, value.NewMap(
			kw("op"), kw("host-field"),
			kw("assignable?"), true,
			kw("field"), value.NewSymbol(mOrF.(*value.Symbol).Name()),
			kw("children"), vec(kw("target")),
		)), nil
	default:
		return merge(n, value.NewMap(
			kw("op"), kw("host-interop"),
			kw("assignable?"), true,
			kw("m-or-f"), value.NewSymbol(mOrF.(*value.Symbol).Name()),
			kw("children"), vec(kw("target")),
		)), nil
	}
}

// (defn parse-let*
//
//	[form env]
//	(into {:op   :let
//	       :form form
//	       :env  env}
//	      (analyze-let form env)))
func (a *Analyzer) parseLetStar(form interface{}, env Env) (ast.Node, error) {
	let, err := a.analyzeLet(form, env)
	if err != nil {
		return nil, err
	}
	return merge(value.NewMap(
		kw("op"), kw("let"),
		kw("form"), form,
		kw("env"), env),
		let), nil
}

// (defn parse-letfn*
//
//	[[_ bindings & body :as form] env]
//	(validate-bindings form env)
//	(let [bindings (apply array-map bindings) ;; pick only one local with the same name, if more are present.
//	      fns      (keys bindings)]
//	  (when-let [[sym] (seq (remove valid-binding-symbol? fns))]
//	    (throw (ex-info (str "Bad binding form: " sym)
//	                    (merge {:form form
//	                            :sym  sym}
//	                           (-source-info form env)))))
//	  (let [binds (reduce (fn [binds name]
//	                        (assoc binds name
//	                               {:op    :binding
//	                                :env   env
//	                                :name  name
//	                                :form  name
//	                                :local :letfn}))
//	                      {} fns)
//	        e (update-in env [:locals] merge binds) ;; pre-seed locals
//	        binds (reduce-kv (fn [binds name bind]
//	                           (assoc binds name
//	                                  (merge bind
//	                                         {:init     (analyze-form (bindings name)
//	                                                                  (ctx e :ctx/expr))
//	                                          :children [:init]})))
//	                         {} binds)
//	        e (update-in env [:locals] merge (update-vals binds dissoc-env))
//	        body (analyze-body body e)]
//	    {:op       :letfn
//	     :env      env
//	     :form     form
//	     :bindings (vec (vals binds)) ;; order is irrelevant
//	     :body     body
//	     :children [:bindings :body]})))
func (a *Analyzer) parseLetfnStar(form interface{}, env Env) (ast.Node, error) {
	if value.Count(form) < 2 {
		return nil, exInfo("letfn requires a binding vector and a body", nil)
	}
	bindings := second(form)
	body := value.Rest(value.Rest(form))
	if err := a.validateBindings(form, env); err != nil {
		return nil, err
	}
	bindingsMap := value.NewMap().(value.Associative)
	for seq := value.Seq(bindings); seq != nil; seq = seq.Next().Next() {
		bindingsMap = bindingsMap.Assoc(value.First(seq), second(seq))
	}
	fns := value.Keys(bindingsMap)
	if sym := value.First(value.Seq(removeP(isValidBindingSymbol, fns))); sym != nil {
		return nil, exInfo("bad binding form: "+value.ToString(sym), nil)
	}
	binds := value.ReduceInit(func(binds, name interface{}) interface{} {
		return value.Assoc(binds, name, value.NewMap(
			kw("op"), kw("binding"),
			kw("env"), env,
			kw("name"), name,
			kw("form"), name,
			kw("local"), kw("letfn")))
	}, value.NewMap(), fns)
	e := updateIn(env, vec(kw("locals")), merge, binds).(Env)
	binds = value.ReduceKV(func(binds, name, bind interface{}) interface{} {
		init, err := a.analyzeForm(value.Get(bindingsMap, name), ctxEnv(e, ctxExpr))
		if err != nil {
			panic(err)
		}
		return value.Assoc(binds, name, merge(bind, value.NewMap(
			kw("init"), init,
			kw("children"), vec(kw("init")))))
	}, value.NewMap(), binds)
	e = updateIn(env, vec(kw("locals")), merge, updateVals(binds, dissocEnv)).(Env)
	body, err := a.analyzeBody(body, e)
	if err != nil {
		return nil, err
	}
	return value.NewMap(
		kw("op"), kw("letfn"),
		kw("env"), env,
		kw("form"), form,
		kw("bindings"), value.Vals(binds.(value.Associative)),
		kw("body"), body,
		kw("children"), vec(kw("bindings"), kw("body"))), nil
}

// (defn parse-loop*
//
//	[form env]
//	(let [loop-id (gensym "loop_") ;; can be used to find matching recur
//	      env (assoc env :loop-id loop-id)]
//	  (into {:op      :loop
//	         :form    form
//	         :env     env
//	         :loop-id loop-id}
//	        (analyze-let form env))))
func (a *Analyzer) parseLoopStar(form interface{}, env Env) (ast.Node, error) {
	loopID := a.Gensym("loop_")
	env = env.Assoc(kw("loop-id"), loopID).(Env)
	loop, err := a.analyzeLet(form, env)
	if err != nil {
		return nil, err
	}
	return merge(value.NewMap(
		kw("op"), kw("loop"),
		kw("form"), form,
		kw("env"), env,
		kw("loop-id"), loopID),
		loop), nil
}

// (defn parse-recur
//
//	[[_ & exprs :as form] {:keys [context loop-locals loop-id]
//	                       :as env}]
//	(when-let [error-msg
//	           (cond
//	            (not (isa? context :ctx/return))
//	            "Can only recur from tail position"
//	            (not (= (count exprs) loop-locals))
//	            (str "Mismatched argument count to recur, expected: " loop-locals
//	                 " args, had: " (count exprs)))]
//	  (throw (ex-info error-msg
//	                  (merge {:exprs exprs
//	                          :form  form}
//	                         (-source-info form env)))))
//	(let [exprs (mapv (analyze-in-env (ctx env :ctx/expr)) exprs)]
//	  {:op          :recur
//	   :env         env
//	   :form        form
//	   :exprs       exprs
//	   :loop-id     loop-id
//	   :children    [:exprs]}))
func (a *Analyzer) parseRecur(form interface{}, env Env) (ast.Node, error) {
	exprs := value.Rest(form)
	ctx := value.Get(env, kw("context"))
	loopLocals := value.Get(env, kw("loop-locals"))
	loopID := value.Get(env, kw("loop-id"))

	errorMsg := ""
	switch {
	case !value.Equal(ctx, ctxReturn):
		panic("Can only recur from tail position")
		errorMsg = "can only recur from tail position"
	case !value.Equal(value.Count(exprs), loopLocals):
		errorMsg = fmt.Sprintf("mismatched argument count to recur, expected: %v args, had: %v", loopLocals, value.Count(exprs))
	}
	if errorMsg != "" {
		return nil, exInfo(errorMsg, nil)
	}
	var exprNodes []interface{}
	for seq := value.Seq(exprs); seq != nil; seq = seq.Next() {
		expr := value.First(seq)
		exprNode, err := a.analyzeForm(expr, ctxEnv(env, ctxExpr))
		if err != nil {
			return nil, err
		}
		exprNodes = append(exprNodes, exprNode)
	}

	return value.NewMap(
		kw("op"), kw("recur"),
		kw("env"), env,
		kw("form"), form,
		kw("exprs"), vec(exprNodes...),
		kw("loop-id"), loopID,
		kw("children"), vec(kw("exprs"))), nil
}

// (defn parse-fn*
//
//	[[op & args :as form] env]
//	(wrapping-meta
//	 (let [[n meths] (if (symbol? (first args))
//	                   [(first args) (next args)]
//	                   [nil (seq args)])
//	       name-expr {:op    :binding
//	                  :env   env
//	                  :form  n
//	                  :local :fn
//	                  :name  n}
//	       e (if n (assoc (assoc-in env [:locals n] (dissoc-env name-expr)) :local name-expr) env)
//	       once? (-> op meta :once boolean)
//	       menv (assoc (dissoc e :in-try) :once once?)
//	       meths (if (vector? (first meths)) (list meths) meths) ;;turn (fn [] ...) into (fn ([]...))
//	       methods-exprs (mapv #(analyze-fn-method % menv) meths)
//	       variadic (seq (filter :variadic? methods-exprs))
//	       variadic? (boolean variadic)
//	       fixed-arities (seq (map :fixed-arity (remove :variadic? methods-exprs)))
//	       max-fixed-arity (when fixed-arities (apply max fixed-arities))]
//	   (when (>= (count variadic) 2)
//	     (throw (ex-info "Can't have more than 1 variadic overload"
//	                     (merge {:variadics (mapv :form variadic)
//	                             :form      form}
//	                            (-source-info form env)))))
//	   (when (not= (seq (distinct fixed-arities)) fixed-arities)
//	     (throw (ex-info "Can't have 2 or more overloads with the same arity"
//	                     (merge {:form form}
//	                            (-source-info form env)))))
//	   (when (and variadic?
//	              (not-every? #(<= (:fixed-arity %)
//	                         (:fixed-arity (first variadic)))
//	                     (remove :variadic? methods-exprs)))
//	     (throw (ex-info "Can't have fixed arity overload with more params than variadic overload"
//	                     (merge {:form form}
//	                            (-source-info form env)))))
//	   (merge {:op              :fn
//	           :env             env
//	           :form            form
//	           :variadic?       variadic?
//	           :max-fixed-arity max-fixed-arity
//	           :methods         methods-exprs
//	           :once            once?}
//	          (when n
//	            {:local name-expr})
//	          {:children (conj (if n [:local] []) :methods)}))))
func (a *Analyzer) parseFnStar(form interface{}, env Env) (ast.Node, error) {
	fnSym, _ := value.First(form).(*value.Symbol)
	args := value.Rest(form)

	var n *value.Symbol
	var meths interface{}
	if sym, ok := value.First(args).(*value.Symbol); ok {
		n = sym
		meths = value.Next(args)
	} else {
		meths = value.Seq(args)
	}
	nameExpr := merge(ast.MakeNode(kw("binding"), n), value.NewMap(
		kw("env"), env,
		kw("local"), kw("fn"),
		kw("name"), n,
	))
	e := env
	if n != nil {
		e = assocIn(env, vec(kw("locals"), n), dissocEnv(nameExpr)).(Env)
		e = value.Assoc(e, kw("local"), nameExpr).(Env)
	}

	once := false
	if fnSym != nil {
		if o, ok := value.Get(fnSym.Meta(), kw("once")).(bool); ok && o {
			once = true
		}
	}
	menv := value.Assoc(value.Dissoc(e, kw("in-try")), kw("once"), once)
	if _, ok := value.First(meths).(*value.Vector); ok {
		meths = value.NewList(meths)
	}
	var methodsExprs []interface{}
	for seq := value.Seq(meths); seq != nil; seq = seq.Next() {
		m, err := a.analyzeFnMethod(value.First(seq), menv.(Env))
		if err != nil {
			return nil, err
		}
		methodsExprs = append(methodsExprs, m)
	}
	variadic := false
	for _, m := range methodsExprs {
		if value.Get(m, kw("variadic?")).(bool) {
			variadic = true
			break
		}
	}
	maxFixedArity := 0
	arities := make(map[int]bool)
	sawVariadic := false
	variadicArity := 0
	for _, m := range methodsExprs {
		arity, ok := value.AsInt(value.Get(m, kw("fixed-arity")))
		if !ok {
			panic("fixed-arity not an int")
		}
		if value.Get(m, kw("variadic?")).(bool) {
			if sawVariadic {
				return nil, errors.New("can't have more than 1 variadic overload")
			}
			sawVariadic = true
			variadicArity = arity
		} else if _, ok := arities[arity]; ok {
			return nil, exInfo("can't have 2 or more overloads with the same arity", nil)
		}
		arities[arity] = true
		if arity > maxFixedArity {
			maxFixedArity = arity
		}
	}
	if sawVariadic && maxFixedArity > variadicArity {
		return nil, exInfo("can't have fixed arity overload with more params than variadic overload", nil)
	}

	var children value.Conjer = vec()
	var localMap value.IPersistentMap
	if n != nil {
		localMap = value.NewMap(kw("local"), nameExpr)
		children = value.Conj(children, kw("local"))
	}
	children = value.Conj(children, kw("methods"))

	node := merge(ast.MakeNode(kw("fn"), form), value.NewMap(
		kw("env"), env,
		kw("variadic?"), variadic,
		kw("max-fixed-arity"), maxFixedArity,
		kw("methods"), vec(methodsExprs...),
		kw("once"), once,
	),
		localMap,
		value.NewMap(kw("children"), children),
	)
	return a.wrappingMeta(node)
}

// {:op   :case
//
//	:doc  "Node for a case* special-form expression"
//	:keys [[:form "`(case* expr shift mask default case-map switch-type test-type skip-check?)`"]
//	       ^:children
//	       [:test "The AST node for the expression to test against"]
//	       ^:children
//	       [:nodes "A vector of :case-node AST nodes representing the test/then clauses of the case* expression"]
//	       ^:children
//	       [:default "An AST node representing the default value of the case expression"]
//	       ]}
func (a *Analyzer) parseCaseStar(form interface{}, env Env) (ast.Node, error) {
	var expr *value.Symbol
	var shift, mask int64
	var defaultExpr interface{}
	var caseMap value.IPersistentMap
	var switchType, testType value.Keyword
	var skipCheck interface{}
	var err error
	if value.Count(form) == 9 {
		err = unpackSeq(value.Rest(form), &expr, &shift, &mask, &defaultExpr, &caseMap, &switchType, &testType, &skipCheck)
	} else {
		err = unpackSeq(value.Rest(form), &expr, &shift, &mask, &defaultExpr, &caseMap, &switchType, &testType)
	}
	if err != nil {
		return nil, exInfo(fmt.Sprintf("case*: %v", err), nil)
	}
	if switchType != kw("compact") && switchType != kw("sparse") {
		return nil, exInfo(fmt.Sprintf("unexpected shift type: %v", switchType), nil)
	}
	if testType != kw("int") && testType != kw("hash-identity") && testType != kw("hash-equiv") {
		return nil, exInfo(fmt.Sprintf("unexpected test type: %v", testType), nil)
	}

	testExpr, err := a.analyzeForm(expr, ctxEnv(env, kw("ctx/expr")))
	if err != nil {
		return nil, err
	}
	defaultExpr, err = a.analyzeForm(defaultExpr, env)
	if err != nil {
		return nil, err
	}

	var nodes []interface{}
	for seq := value.Seq(caseMap); seq != nil; seq = seq.Next() {
		// TODO: is the shift, mask, etc. relevant for anything but
		// performance? omitting for now.
		entry := value.First(seq).(value.IMapEntry).Val()
		cond, then := value.First(entry), second(entry)
		// TODO: support a vector of conditions
		condExpr, err := a.analyzeConst(cond, ctxEnv(env, kw("ctx/expr")))
		if err != nil {
			return nil, err
		}
		thenExpr, err := a.analyzeForm(then, env)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, value.NewMap(
			kw("op"), kw("case-node"),
			kw("env"), env,
			kw("tests"), vec(condExpr),
			kw("then"), thenExpr,
		))
	}

	node := merge(ast.MakeNode(kw("case"), form), value.NewMap(
		kw("env"), env,
		kw("test"), testExpr,
		kw("nodes"), vec(nodes...),
		kw("default"), defaultExpr,
	),
		value.NewMap(kw("children"), vec(kw("test"), kw("nodes"), kw("default"))),
	)
	return node, nil
}

// (defn analyze-fn-method [[params & body :as form] {:keys [locals local] :as env}]
//
//	(when-not (vector? params)
//	  (throw (ex-info "Parameter declaration should be a vector"
//	                  (merge {:params params
//	                          :form   form}
//	                         (-source-info form env)
//	                         (-source-info params env)))))
//	(when (not-every? valid-binding-symbol? params)
//	  (throw (ex-info (str "Params must be valid binding symbols, had: "
//	                       (mapv class params))
//	                  (merge {:params params
//	                          :form   form}
//	                         (-source-info form env)
//	                         (-source-info params env))))) ;; more specific
//	(let [variadic? (boolean (some '#{&} params))
//	      params-names (if variadic? (conj (pop (pop params)) (peek params)) params)
//	      env (dissoc env :local)
//	      arity (count params-names)
//	      params-expr (mapv (fn [name id]
//	                          {:env       env
//	                           :form      name
//	                           :name      name
//	                           :variadic? (and variadic?
//	                                           (= id (dec arity)))
//	                           :op        :binding
//	                           :arg-id    id
//	                           :local     :arg})
//	                        params-names (range))
//	      fixed-arity (if variadic?
//	                    (dec arity)
//	                    arity)
//	      loop-id (gensym "loop_")
//	      body-env (into (update-in env [:locals]
//	                                merge (zipmap params-names (map dissoc-env params-expr)))
//	                     {:context     :ctx/return
//	                      :loop-id     loop-id
//	                      :loop-locals (count params-expr)})
//	      body (analyze-body body body-env)]
//	  (when variadic?
//	    (let [x (drop-while #(not= % '&) params)]
//	      (when (contains? #{nil '&} (second x))
//	        (throw (ex-info "Invalid parameter list"
//	                        (merge {:params params
//	                                :form   form}
//	                               (-source-info form env)
//	                               (-source-info params env)))))
//	      (when (not= 2 (count x))
//	        (throw (ex-info (str "Unexpected parameter: " (first (drop 2 x))
//	                             " after variadic parameter: " (second x))
//	                        (merge {:params params
//	                                :form   form}
//	                               (-source-info form env)
//	                               (-source-info params env)))))))
//	  (merge
//	   {:op          :fn-method
//	    :form        form
//	    :loop-id     loop-id
//	    :env         env
//	    :variadic?   variadic?
//	    :params      params-expr
//	    :fixed-arity fixed-arity
//	    :body        body
//	    :children    [:params :body]}
//	   (when local
//	     {:local (dissoc-env local)}))))
func (a *Analyzer) analyzeFnMethod(form interface{}, env Env) (ast.Node, error) {
	if _, ok := form.(value.ISeqable); !ok {
		return nil, exInfo("invalid fn method", nil)
	}
	params, ok := value.First(form).(value.IPersistentVector)
	if !ok {
		return nil, exInfo("parameter declaration should be a vector", nil)
	}
	body := value.Rest(form)

	var variadic bool
	var variadicParams value.ISeq
	for seq := value.Seq(params); seq != nil; seq = seq.Next() {
		if !isValidBindingSymbol(seq.First()) {
			return nil, exInfo(fmt.Sprintf("params must be valid binding symbols, had: %T", seq.First()), nil)
		}
		if seq.First().(*value.Symbol).Name() == "&" {
			if variadic {
				return nil, exInfo("can't have more than 1 variadic param", nil)
			}
			variadic = true
			variadicParams = seq.Next()
		}
	}
	paramsNames := params
	if variadic {
		if value.Count(variadicParams) != 1 {
			return nil, exInfo("variadic method must have exactly 1 param", nil)
		}
		paramsNames = params.Pop().Pop().(value.Conjer).Conj(params.Peek()).(value.IPersistentVector)
	}
	env = value.Dissoc(env, kw("local")).(Env)
	arity := paramsNames.Count()
	var paramsExpr value.IPersistentVector = vec()
	id := 0
	for seq := value.Seq(paramsNames); seq != nil; seq, id = seq.Next(), id+1 {
		name := seq.First()
		paramsExpr = paramsExpr.Cons(value.NewMap(
			kw("env"), env,
			kw("form"), name,
			kw("name"), name,
			kw("variadic?"), variadic && id == arity-1,
			kw("op"), kw("binding"),
			kw("arg-id"), id,
			kw("local"), kw("arg"),
		)).(value.IPersistentVector)
	}
	fixedArity := arity
	if variadic {
		fixedArity = arity - 1
	}
	loopID := a.Gensym("loop_")
	var bodyEnv Env
	{
		var localsMap value.IPersistentMap = value.NewMap()
		if locals, ok := value.Get(env, kw("locals")).(value.IPersistentMap); ok {
			localsMap = locals
		}
		for i := 0; i < paramsNames.Count(); i++ {
			localsMap = localsMap.Assoc(value.MustNth(paramsNames, i), dissocEnv(value.MustNth(paramsExpr, i).(value.IPersistentMap))).(value.IPersistentMap)
		}
		bodyEnv = env.Assoc(kw("locals"), localsMap).(Env)
	}
	bodyEnv = merge(bodyEnv,
		value.NewMap(
			kw("context"), kw("ctx/return"),
			kw("loop-id"), loopID,
			kw("loop-locals"), paramsExpr.Count(),
		),
	).(Env)
	bodyNode, err := a.analyzeBody(body, bodyEnv)
	if err != nil {
		return nil, err
	}

	node := merge(ast.MakeNode(kw("fn-method"), form), value.NewMap(
		kw("loop-id"), loopID, // TODO
		kw("env"), env,
		kw("variadic?"), variadic,
		kw("params"), paramsExpr,
		kw("fixed-arity"), fixedArity,
		kw("body"), bodyNode,
		kw("children"), vec(kw("params"), kw("body")),
	))
	return node, nil
}

// (defn parse-var
//
//	[[_ var :as form] env]
//	(when-not (= 2 (count form))
//	  (throw (ex-info (str "Wrong number of args to var, had: " (dec (count form)))
//	                  (merge {:form form}
//	                         (-source-info form env)))))
//	(if-let [var (resolve-sym var env)]
//	  {:op   :the-var
//	   :env  env
//	   :form form
//	   :var  var}
//	  (throw (ex-info (str "var not found: " var) {:var var}))))
func (a *Analyzer) parseVar(form interface{}, env Env) (ast.Node, error) {
	vrSym := second(form)
	if value.Count(form) != 2 {
		return nil, exInfo(fmt.Sprintf("wrong number of args to var, had: %d", value.Count(form)-1), nil)
	}
	vr := a.resolveSym(vrSym, env)
	if vr == nil {
		return nil, exInfo(fmt.Sprintf("var not found: %s", vrSym), nil)
	}
	return merge(ast.MakeNode(kw("the-var"), form), value.NewMap(
		kw("env"), env,
		kw("var"), vr,
	)), nil
}

// (defn wrapping-meta
//
//	[{:keys [form env] :as expr}]
//	(let [meta (meta form)]
//	  (if (and (obj? form)
//	           (seq meta))
//	    {:op       :with-meta
//	     :env      env
//	     :form     form
//	     :meta     (analyze-form meta (ctx env :ctx/expr))
//	     :expr     (assoc-in expr [:env :context] :ctx/expr)
//	     :children [:meta :expr]}
//	    expr)))
func (a *Analyzer) wrappingMeta(expr ast.Node) (ast.Node, error) {
	form := ast.Form(expr)
	env := value.Get(expr, kw("env")).(Env)
	var meta value.IPersistentMap
	if m, ok := form.(value.IMeta); ok {
		meta = m.Meta()
	}
	if value.Seq(meta) == nil {
		return expr, nil
	}
	metaNode, err := a.analyzeForm(meta, ctxEnv(env, ctxExpr))
	if err != nil {
		return nil, err
	}
	exprNode := assocIn(expr, vec(kw("env"), kw("context")), ctxExpr)
	n := ast.MakeNode(kw("with-meta"), form)
	return merge(n, value.NewMap(
		kw("env"), env,
		kw("meta"), metaNode,
		kw("expr"), exprNode,
		kw("children"), vec(kw("meta"), kw("expr")),
	)), nil
}

////////////////////////////////////////////////////////////////////////////////
// Helpers

// (defn resolve-ns
//
//	"Resolves the ns mapped by the given sym in the global env"
//	[ns-sym {:keys [ns]}]
//	(when ns-sym
//	  (let [namespaces (:namespaces (env/deref-env))]
//	    (or (get-in namespaces [ns :aliases ns-sym])
//	        (:ns (namespaces ns-sym))))))
func (a *Analyzer) resolveNS(nsSym interface{}, env Env) *value.Symbol {
	ns := value.Get(env, kw("ns")).(*value.Symbol)
	if nsSym == nil {
		return nil
	}
	globalEnv := a.GlobalEnv.Deref()
	namespaces, _ := value.Get(globalEnv, kw("namespaces")).(value.IPersistentMap)
	if res := value.Get(value.Get(value.Get(namespaces, ns), kw("aliases")), nsSym); res != nil {
		if sym, ok := res.(*value.Symbol); ok {
			return sym
		}
		return nil
	}
	if sym, ok := value.Get(value.Get(namespaces, nsSym), kw("ns")).(*value.Symbol); ok {
		return sym
	}
	return nil
}

// (defn resolve-sym
//
//	"Resolves the value mapped by the given sym in the global env"
//	[sym {:keys [ns] :as env}]
//	(when (symbol? sym)
//	  (let [sym-ns (when-let [ns (namespace sym)]
//	                 (symbol ns))
//	        full-ns (resolve-ns sym-ns env)]
//	    (when (or (not sym-ns) full-ns)
//	      (let [name (if sym-ns (-> sym name symbol) sym)]
//	        (-> (env/deref-env) :namespaces (get (or full-ns ns)) :mappings (get name)))))))
func (a *Analyzer) resolveSym(symIfc interface{}, env Env) interface{} {
	ns, _ := value.Get(env, kw("ns")).(*value.Symbol)

	sym, ok := symIfc.(*value.Symbol)
	if !ok {
		return nil
	}
	var symNS *value.Symbol
	if sym.Namespace() != "" {
		symNS = value.NewSymbol(sym.Namespace())
	}
	fullNS := a.resolveNS(symNS, env)
	if fullNS == nil && symNS != nil {
		return nil
	}

	var name *value.Symbol
	if symNS != nil {
		name = value.NewSymbol(sym.Name())
	} else {
		name = sym
	}

	if fullNS != nil {
		ns = fullNS
	}
	globalEnv := a.GlobalEnv.Deref()
	namespaces, _ := value.Get(globalEnv, kw("namespaces")).(value.IPersistentMap)
	nsMap, _ := value.Get(namespaces, ns).(value.IPersistentMap)
	mappings, _ := value.Get(nsMap, kw("mappings")).(value.IPersistentMap)
	return value.Get(mappings, name)
}

// (defn validate-bindings
//
//	[[op bindings & _ :as form] env]
//	(when-let [error-msg
//	           (cond
//	            (not (vector? bindings))
//	            (str op " requires a vector for its bindings, had: "
//	                 (class bindings))
//	            (not (even? (count bindings)))
//	            (str op " requires an even number of forms in binding vector, had: "
//	                 (count bindings)))]
//	  (throw (ex-info error-msg
//	                  (merge {:form     form
//	                          :bindings bindings}
//	                         (-source-info form env))))))
func (a *Analyzer) validateBindings(form interface{}, env Env) error {
	op := value.First(form)
	bindings, ok := second(form).(value.IPersistentVector)
	errMsg := ""
	switch {
	case !ok:
		errMsg = fmt.Sprintf("%s requires a vector for its bindings, had: %T", op, bindings)
	case value.Count(bindings)%2 != 0:
		errMsg = fmt.Sprintf("%s requires an even number of forms in binding vector, had: %d", op, value.Count(bindings))
	}
	if errMsg == "" {
		return nil
	}
	return exInfo(errMsg, nil)
}

func kw(s string) value.Keyword {
	return value.NewKeyword(s)
}

func vec(v ...interface{}) *value.Vector {
	return value.NewVector(v...)
}

func second(x interface{}) interface{} {
	return value.First(value.Rest(x))
}

func exInfo(errStr string, _ interface{}) error {
	// TODO
	return fmt.Errorf(errStr)
}

func withRawForm(n ast.Node, form interface{}) ast.Node {
	rawFormsKV := n.EntryAt(kw("raw-forms"))
	if rawFormsKV == nil {
		return n
	}
	if rf, ok := rawFormsKV.Val().(value.Conjer); ok {
		return n.Assoc(kw("raw-forms"), value.Conj(rf, form))
	}
	return n
}

func mapWhen(k, v interface{}) value.IPersistentMap {
	if v == nil {
		return nil
	}
	return value.NewMap(k, v)
}

func merge(as ...interface{}) value.Associative {
	if len(as) == 0 {
		return nil
	}

	var res value.Associative
	i := 0
	for {
		if i >= len(as) {
			return nil
		}
		if as[i] != nil {
			break
		}
		i++
	}
	res = as[i].(value.Associative)
	for _, a := range as[i+1:] {
		for seq := value.Seq(a); seq != nil; seq = seq.Next() {
			entry := seq.First().(value.IMapEntry)
			res = value.Assoc(res, entry.Key(), entry.Val())
		}
	}
	return res
}

func remove(v interface{}, coll interface{}) interface{} {
	if coll == nil {
		return nil
	}
	var items []interface{}
	for seq := value.Seq(coll); seq != nil; seq = seq.Next() {
		if !value.Equal(v, seq.First()) {
			items = append(items, seq.First())
		}
	}
	return vec(items...)
}

func removeP(fn func(interface{}) bool, coll interface{}) interface{} {
	if coll == nil {
		return nil
	}
	var items []interface{}
	for seq := value.Seq(coll); seq != nil; seq = seq.Next() {
		if !fn(seq.First()) {
			items = append(items, seq.First())
		}
	}
	return vec(items...)
}

func ctxEnv(env Env, ctx value.Keyword) Env {
	return value.Assoc(env, kw("context"), ctx).(Env)
}

func assocIn(mp interface{}, keys interface{}, val interface{}) value.Associative {
	count := value.Count(keys)
	if count == 0 {
		return value.Assoc(mp, nil, val)
	} else if count == 1 {
		return value.Assoc(mp, value.First(keys), val)
	}
	return value.Assoc(mp, value.First(keys),
		assocIn(value.Get(mp, value.First(keys)), value.Rest(keys), val))
}

func updateIn(mp interface{}, keys interface{}, fn func(...interface{}) value.Associative, args ...interface{}) value.Associative {
	var up func(mp interface{}, keys interface{}, fn func(...interface{}) value.Associative, args []interface{}) value.Associative
	up = func(mp interface{}, keys interface{}, fn func(...interface{}) value.Associative, args []interface{}) value.Associative {
		k := value.First(keys)
		ks := value.Rest(keys)
		if value.Count(ks) > 0 {
			return value.Assoc(mp, k, up(value.Get(mp, k), ks, fn, args))
		} else {
			vals := []interface{}{value.Get(mp, k)}
			vals = append(vals, args...)
			return value.Assoc(mp, k, fn(vals...))
		}
	}
	return up(mp, keys, fn, args)
}

func dissocEnv(node interface{}) interface{} {
	return value.Dissoc(node, kw("env"))
}

func isValidBindingSymbol(v interface{}) bool {
	sym, ok := v.(*value.Symbol)
	if !ok {
		return false
	}
	if sym.Namespace() != "" {
		return false
	}
	if strings.Contains(sym.Name(), ".") {
		return false
	}
	return true
}

func classifyType(v interface{}) value.Keyword {
	switch v.(type) {
	case nil:
		return kw("nil")
	case bool:
		return kw("bool")
	case value.Keyword:
		return kw("keyword")
	case *value.Symbol:
		return kw("symbol")
	case string:
		return kw("string")
	case value.IPersistentVector:
		return kw("vector")
	case value.IPersistentMap:
		return kw("map")
	case value.IPersistentSet:
		return kw("set")
	case value.ISeq:
		return kw("seq")
	case *value.Char:
		return kw("char")
	case *regexp.Regexp:
		return kw("regex")
	case *value.Var:
		return kw("var")
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		*value.BigInt, *value.BigDecimal, *value.Ratio:
		return kw("number")
	default:
		return kw("unknown")

		// TODO: type, record, class
	}
}

// (defn ^:private split-with' [pred coll]
//
//	(loop [take [] drop coll]
//	  (if (seq drop)
//	    (let [[el & r] drop]
//	      (if (pred el)
//	        (recur (conj take el) r)
//	        [(seq take) drop]))
//	    [(seq take) ()])))
func splitWith(pred func(interface{}) bool, coll interface{}) (interface{}, interface{}) {
	var take value.Conjer = vec()
	drop := coll
	for {
		seq := value.Seq(drop)
		if seq == nil {
			return value.Seq(take), value.NewList()
		}
		el := value.First(drop)
		if pred(el) {
			take = value.Conj(take, el)
			drop = value.Rest(drop)
		} else {
			return value.Seq(take), drop
		}
	}
}

func updateVals(m interface{}, fn func(interface{}) interface{}) interface{} {
	in := m
	if in == nil {
		in = value.NewMap()
	}
	return value.ReduceKV(func(m interface{}, k interface{}, v interface{}) interface{} {
		return value.Assoc(m, k, fn(v))
	}, value.NewMap(), in)
}

func unpackSeq(s interface{}, dsts ...interface{}) error {
	seq := value.Seq(s)
	for i, d := range dsts {
		if seq == nil {
			return fmt.Errorf("not enough arguments, got %d, expected %d", i, len(dsts))
		}
		dst := reflect.ValueOf(d)

		v := value.First(seq)
		if v == nil {
			if dst.Elem().Kind() != reflect.Interface {
				return fmt.Errorf("cannot assign nil to %s", dst.Type())
			}
			continue // leave nil
		}

		val := reflect.ValueOf(v)
		seq = seq.Next()
		if !val.Type().AssignableTo(dst.Elem().Type()) {
			return fmt.Errorf("argument %d: expected %s, got %s", i, dst.Elem().Type(), val.Type())
		}
		dst.Elem().Set(val)
	}
	return nil
}
