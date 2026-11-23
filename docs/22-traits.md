# 22. Traits

A trait is a named set of method signatures. An `impl Trait for Type` supplies
the bodies. Generic functions can put a trait bound on a type parameter, and
calls through that parameter are resolved to the concrete type's method when
the function is monomorphized. There is no runtime dispatch table, no `dyn
Trait`, and no associated types: this is the smallest useful trait system that
fits the compiler's existing monomorphization model.

```zx
trait Describe {
    fn describe(&self) -> str;
}

struct Dog { name: str }

impl Describe for Dog {
    fn describe(&self) -> str { return "dog"; }
}

fn announce<T: Describe>(x: T) -> str {
    return x.describe();
}

fn main() -> i32 {
    println(announce(Dog { name: "rex" })); // dog
    0
}
```

## 22.1 What is checked

- A trait method must have a receiver (`&self`, `&mut self`, or `self`) and no
  body. A body in a trait declaration is `E0207`.
- An `impl Trait for T` must define exactly the trait's methods, with the same
  receivers and argument counts. An unknown method is `E0406`, a wrong
  signature is `E0053`, and a missing method is `E0046`.
- A bound must name a defined trait (`E0405` otherwise). When a generic is
  instantiated, each concrete type argument is checked for the trait's methods
  (`E0277`); the call in the body then resolves normally. Since templates are
  not body-checked, an unsatisfied bound can also surface as `E0599` at the
  call site.
- Generic impls work in the same shape as inherent generic impls:
  `impl<T> Describe for Wrap<T>`.

The trait name is only used for validation and grouping. Method lookup uses the
ordinary method table on the concrete type, so two traits with the same method
name on one type collide (`E0592`) — use distinct names.

## 22.2 Limits

- No trait bounds on `impl` blocks, no supertraits, no default method bodies.
- No `dyn Trait`, associated types, const generics, or operator overloading
  through traits.
- Trait methods cannot themselves be generic (`fn m<T>(...)`).
- Bounds are checked on the concrete type at monomorphization, not proved over
  all possible `T` at the generic definition. That matches the rest of the
  generic implementation: a template is checked once and bodies are checked
  per instantiation.

`tests/traits.zx` is the acceptance test; `tests/errors/trait_bound.zx` pins an
unsatisfied bound.
