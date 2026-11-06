'use strict';

// node-resolve 16.0.3 only calls deepmerge({}, defaults), then freezes the
// result. Its user options use object spread. This is intentionally a clone
// adapter, not a general merge library. Fail if that upstream contract changes.
module.exports = function cloneRollupDefaults(target, source) {
  if (arguments.length !== 2 ||
      target === null || Object.getPrototypeOf(target) !== Object.prototype ||
      Reflect.ownKeys(target).length !== 0 ||
      source === null || Object.getPrototypeOf(source) !== Object.prototype) {
    throw new TypeError('Rollup defaults clone requires an empty plain target and a plain source');
  }
  return structuredClone(source);
};
