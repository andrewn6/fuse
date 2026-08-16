// string_enum generates a closed-vocabulary wire enum with an Other escape
// hatch: known values round-trip as variants, unknown values pass through
// verbatim instead of failing to decode, so an older sdk keeps working
// against a newer server.
macro_rules! string_enum {
    (
        $(#[$meta:meta])*
        $vis:vis enum $name:ident {
            $(
                $(#[$vmeta:meta])*
                $variant:ident => $value:literal
            ),* $(,)?
        }
    ) => {
        $(#[$meta])*
        #[derive(Debug, Clone, PartialEq, Eq, Hash)]
        #[non_exhaustive]
        $vis enum $name {
            $(
                $(#[$vmeta])*
                $variant,
            )*
            /// A value this SDK does not know yet, carried through verbatim.
            Other(String),
        }

        impl $name {
            /// Returns the wire representation of this value.
            pub fn as_str(&self) -> &str {
                match self {
                    $(Self::$variant => $value,)*
                    Self::Other(value) => value,
                }
            }
        }

        impl ::std::fmt::Display for $name {
            fn fmt(&self, f: &mut ::std::fmt::Formatter<'_>) -> ::std::fmt::Result {
                f.write_str(self.as_str())
            }
        }

        impl ::std::convert::From<&str> for $name {
            fn from(value: &str) -> Self {
                match value {
                    $($value => Self::$variant,)*
                    other => Self::Other(other.to_owned()),
                }
            }
        }

        impl ::std::convert::From<String> for $name {
            fn from(value: String) -> Self {
                match value.as_str() {
                    $($value => Self::$variant,)*
                    _ => Self::Other(value),
                }
            }
        }

        impl ::serde::Serialize for $name {
            fn serialize<S: ::serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
                serializer.serialize_str(self.as_str())
            }
        }

        impl<'de> ::serde::Deserialize<'de> for $name {
            fn deserialize<D: ::serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
                let value = <String as ::serde::Deserialize>::deserialize(deserializer)?;
                Ok(Self::from(value))
            }
        }
    };
}

pub(crate) use string_enum;
